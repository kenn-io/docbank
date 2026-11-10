package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "wait" {
		wait()
		return
	}
	allocation := make([]byte, 3<<30)
	for i := 0; i < len(allocation); i += 4096 {
		allocation[i] = 1
	}
	fmt.Println("unexpected allocation success")
	runtime.KeepAlive(allocation)
}

// wait connects to the address the test sends on stdin, which tells the test
// that the probe is running, then blocks until it is killed.
func wait() {
	address, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(2)
	}
	connection, err := net.Dial("tcp", string(address))
	if err != nil {
		os.Exit(3)
	}
	_, _ = io.Copy(io.Discard, connection)
	os.Exit(4)
}
