package main

import (
	"fmt"
	"github.com/pytogo/pytogo/portforward"
	"time"
)

func main() {
	err := portforward.Forward("test", "broken", 4000, 80, "/Users/sebastianziemann/.kube/config", 0, "")

	if err != nil {
		fmt.Println(err.Error())
	}

	time.Sleep(1 * time.Minute)
}
