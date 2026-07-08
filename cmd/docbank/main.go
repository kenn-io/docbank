package main

import (
	"fmt"
	"os"

	"go.kenn.io/docbank/cmd/docbank/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
