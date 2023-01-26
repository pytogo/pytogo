package main

import (
	"fmt"
	"github.com/pytogo/pytogo/portforward"
)

func main() {
	err := portforward.Forward("test", "web-svc", 4000, 4000, "/Users/sebastianziemann/.kube/config", 0, "")

	fmt.Println(err.Error())

}
