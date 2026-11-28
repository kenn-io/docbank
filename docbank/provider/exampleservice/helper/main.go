// Command helper is the example hosting service's provider helper. See
// package exampleservice.
package main

import (
	"os"

	"go.kenn.io/docbank/docbank/provider/exampleservice"
)

func main() {
	os.Exit(exampleservice.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
