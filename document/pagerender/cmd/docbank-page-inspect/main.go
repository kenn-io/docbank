// docbank-page-inspect is the bounded page geometry bridge. Its caller must
// install operating-system memory and time limits before starting it.
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"

	"go.kenn.io/docbank/document/pagerender"
	"go.kenn.io/docbank/internal/canonical"
)

func main() {
	version := flag.Bool("version", false, "print runtime identity")
	protocol := flag.String("protocol", "", "inspection protocol")
	versionID := flag.String("version-id", "", "exact source version")
	mediaType := flag.String("media-type", "", "source media type")
	flag.Parse()
	if *version {
		fmt.Println(pagerender.Protocol + " " + runtime.Version() + " pdfcpu/v0.15.0")
		return
	}
	if *protocol != pagerender.Protocol || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unsupported inspection protocol")
		os.Exit(2)
	}
	result, err := pagerender.InspectInput(os.Stdin, *versionID, *mediaType)
	if err != nil {
		fmt.Fprintln(os.Stderr, "source geometry unavailable")
		os.Exit(1)
	}
	encoded, err := canonical.Marshal(result)
	if err != nil || len(encoded) > 16<<20 {
		fmt.Fprintln(os.Stderr, "inspection output exceeds bounds")
		os.Exit(1)
	}
	if _, err := os.Stdout.Write(encoded); err != nil {
		os.Exit(1)
	}
}
