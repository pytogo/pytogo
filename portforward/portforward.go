package portforward

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"

	// Auth plugins - common and cloud provider
	_ "github.com/Azure/go-autorest/autorest/adal"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
)

// ===== Management of open connections =====

/*
Thoughts:

Global states are bad but should any reference to memory exists in
the Python and Go space? Who should when free the memory?

Every space should keep the ownership of its memory allocations.
Parameters are passed from Python to Go but Go never owns them.
*/
var (
	activeForwards = make(map[string]chan struct{})
	mutex          sync.Mutex
)

// registerForwarding adds a forwarding to the active forwards.
func registerForwarding(namespace, podOrService string, stopCh chan struct{}) {
	key := fmt.Sprintf("%s/%s", namespace, podOrService)

	mutex.Lock()
	defer mutex.Unlock()

	if otherCh, ok := activeForwards[key]; ok {
		close(otherCh)
	}

	activeForwards[key] = stopCh
}

// StopForwarding closes a port forwarding.
func StopForwarding(namespace, podOrService string) {
	key := fmt.Sprintf("%s/%s", namespace, podOrService)

	mutex.Lock()
	defer mutex.Unlock()

	if otherCh, ok := activeForwards[key]; ok {
		close(otherCh)
		delete(activeForwards, key)
	}
}

// ===== Port forwarding =====

// Forward connects to a pod/service and tunnels traffic from a local port to this pod.
func Forward(namespace, podOrService string, fromPort, toPort int, configPath string, logLevel int, kubeContext string) error {
	// LOGGING
	log := newLogger(logLevel)
	overwriteLog(log)

	// Based on example https://github.com/kubernetes/client-go/issues/51#issuecomment-436200428

	// CONFIG
	config, err := loadConfig(configPath, kubeContext, log)

	if err != nil {
		return err
	}

	// PREPARE
	// Check & prepare name
	// PortForward must be started in a go-routine, therefore we have
	// to check manually if the pod or service exists and is reachable.
	resType, err := prepareForward(config, namespace, podOrService)

	if err != nil {
		return err
	}

	// DIALER
	dialer, err := newDialer(config, namespace, resType, podOrService)

	if err != nil {
		return err
	}

	// PORT FORWARD
	stopChan, readyChan := make(chan struct{}, 1), make(chan struct{}, 1)

	ports := fmt.Sprintf("%d:%d", fromPort, toPort)

	if err := startForward(dialer, ports, stopChan, readyChan, log); err != nil {
		return err
	}

	// HANDLE CLOSING
	registerForwarding(namespace, podOrService, stopChan)
	closeOnSigterm(namespace, podOrService)

	return nil
}

// loadConfig fetches the config from .kube config folder inside the home dir.
func loadConfig(kubeconfigPath string, kubeContext string, log logger) (*rest.Config, error) {
	var configOverrides *clientcmd.ConfigOverrides

	if kubeContext != "" {
		log.Debug("Override kube context with " + kubeContext)
		configOverrides = &clientcmd.ConfigOverrides{
			ClusterInfo: clientcmdapi.Cluster{Server: ""}, CurrentContext: kubeContext,
		}
	} else {
		configOverrides = &clientcmd.ConfigOverrides{ClusterInfo: clientcmdapi.Cluster{Server: ""}}
	}

	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfigPath},
		configOverrides).ClientConfig()

	if err != nil {
		return nil, err
	}

	return config, nil
}

// prepareForward checks whether a pod or service exists and prefixes the resource name.
// (e.g. pod name "web" becomes "pods/web" or the service "db" becomes "services/db".)
func prepareForward(config *rest.Config, namespace, podOrService string) (string, error) {
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return "", err
	}

	_, err = clientset.CoreV1().Services(namespace).Get(context.Background(), podOrService, metav1.GetOptions{})
	if err != nil {
		// We return only errors that NOT represent a "not-found" error
		if !strings.Contains(err.Error(), "not found") {
			return "", err
		}
	} else {
		return "services", nil
	}

	_, err = clientset.CoreV1().Pods(namespace).Get(context.Background(), podOrService, metav1.GetOptions{})
	if err != nil {
		// We return only errors that NOT represent a "not-found" error
		if !strings.Contains(err.Error(), "not found") {
			return "", err
		}
	} else {
		return "pods", nil
	}

	msg := fmt.Sprintf("no service or pod with name %s found", podOrService)
	err = errors.New(msg)

	return "", err
}

// newDialer creates a dialer that connects to the pod.
func newDialer(config *rest.Config, namespace, resType, podOrService string) (httpstream.Dialer, error) {
	roundTripper, upgrader, err := spdy.RoundTripperFor(config)
	if err != nil {
		return nil, err
	}

	path := fmt.Sprintf("/api/v1/namespaces/%s/%s/%s/portforward", namespace, resType, podOrService)
	hostIP := strings.TrimLeft(config.Host, "https://")

	// When there is a "/" in the hostIP, it contains also a path
	if parts := strings.SplitN(hostIP, "/", 2); len(parts) == 2 {
		hostIP = parts[0]
		path = fmt.Sprintf("/%s%s", parts[1], path)
	}

	serverURL := url.URL{Scheme: "https", Path: path, Host: hostIP}

	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: roundTripper}, http.MethodPost, &serverURL)

	return dialer, nil
}

// startForward runs the port-forwarding.
func startForward(dialer httpstream.Dialer, ports string, stopChan, readyChan chan struct{}, log logger) error {
	out, errOut := new(bytes.Buffer), new(bytes.Buffer)

	forwarder, err := portforward.New(dialer, []string{ports}, stopChan, readyChan, out, errOut)
	if err != nil {
		return err
	}

	go func() {
		// Kubernetes will close this channel when it has something to tell us.
		for range readyChan {
		}
		if len(errOut.String()) != 0 {
			panic(errOut.String())
		} else if len(out.String()) != 0 {
			log.Debug(out.String())
		}
	}()

	// Locks until stopChan is closed.
	go func() {
		if err = forwarder.ForwardPorts(); err != nil {
			panic(err)
		}
	}()

	return nil
}

// closeOnSigterm cares about closing a channel when the OS sends a SIGTERM.
func closeOnSigterm(namespace, qualifiedName string) {
	sigs := make(chan os.Signal, 1)

	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		// Received kill signal
		<-sigs

		StopForwarding(namespace, qualifiedName)
	}()
}

func overwriteLog(log logger) {
	if !log.isOff() {
		return
	}

	debugPortforward("Turned off k8s runtime error handlers")
	runtime.ErrorHandlers = make([]func(error), 0)
}

// ===== logger =====

const (
	Debug = iota
	Info
	Warn
	Error
	Off
)

type logger struct {
	level int
}

func newLogger(level int) logger {
	debugPortforward(fmt.Sprintf("level=%d", level))
	return logger{level: level}
}

func (l *logger) Debug(msg string) {
	if l.level > Debug {
		return
	}

	fmt.Printf("DEBUG: %s\n", msg)
}

func (l *logger) Info(msg string) {
	if l.level > Info {
		return
	}

	fmt.Printf("INFO: %s\n", msg)
}

func (l *logger) Warn(msg string) {
	if l.level > Warn {
		return
	}

	fmt.Printf("WARN: %s\n", msg)
}

func (l *logger) Error(msg string) {
	if l.level > Error {
		return
	}

	fmt.Printf("ERROR: %s\n", msg)
}

func (l *logger) isOff() bool {
	return l.level == Off
}

func (l *logger) logError(err error) {
	l.Error(err.Error())
}

func debugPortforward(msg string) {
	if os.Getenv("PORTFORWARD_DEBUG") == "YES" {
		fmt.Println(msg)
	}
}
