//go:build ignore

package main

import (
	"os"
	"path/filepath"

	"github.com/go-pdf/fpdf"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
)

func main() {
	pdf := fpdf.New("P", "pt", "Letter", "")
	pdf.AddUTF8FontFromBytes("regular", "", goregular.TTF)
	pdf.AddUTF8FontFromBytes("bold", "", gobold.TTF)
	pdf.SetFont("regular", "", 12)
	pdf.AddPage()
	pdf.Text(72, 72, "café naïve résumé Ελληνικά")
	pdf.AddPage()
	pdf.Text(72, 72, "Alpha café")
	pdf.SetFont("bold", "", 12)
	pdf.Text(160, 72, "Beta résumé")
	if err := pdf.OutputFileAndClose(filepath.Join(os.Args[1], "subsets-selection.pdf")); err != nil {
		panic(err)
	}
}
