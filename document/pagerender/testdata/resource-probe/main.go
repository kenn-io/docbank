package main

import (
	"fmt"
	"os"
	"runtime"
	"time"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "wait" {
		time.Sleep(time.Hour)
		return
	}
	allocation := make([]byte, 3<<30)
	for i := 0; i < len(allocation); i += 4096 {
		allocation[i] = 1
	}
	fmt.Println("unexpected allocation success")
	runtime.KeepAlive(allocation)
}
