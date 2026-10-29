//go:build ignore

package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
	"github.com/go-pdf/fpdf"
	"go.kenn.io/docbank/document/pagerender"
)

func check(err error) {
	if err != nil {
		panic(err)
	}
}

func pin(path string) pagerender.Executable {
	resolved, err := filepath.EvalSymlinks(path)
	check(err)
	data, err := os.ReadFile(resolved)
	check(err)
	return pagerender.Executable{Path: resolved, SHA256: fmt.Sprintf("%x", sha256.Sum256(data))}
}

func main() {
	if len(os.Args) != 5 {
		panic("usage: bates-recovery-fixture <workspace> <inspector> <renderer> <limiter>")
	}
	root := os.Args[1]
	check(os.MkdirAll(filepath.Join(root, "vault"), 0700))
	pdf := fpdf.New("P", "pt", "Letter", "")
	pdf.SetFont("Helvetica", "", 12)
	for _, text := range []string{"SECRET KEEP SECRET", "SECRET SECOND PAGE"} {
		pdf.AddPage()
		pdf.Text(72, 72, text)
	}
	var source bytes.Buffer
	check(pdf.Output(&source))
	check(os.WriteFile(filepath.Join(root, "two-pages.pdf"), source.Bytes(), 0600))
	profile := pagerender.Profile{Inspector: pin(os.Args[2]), Renderer: pin(os.Args[3]),
		Limiter: pin(os.Args[4]), DeploymentIdentity: "synthetic-browser-recovery"}
	check(profile.Validate())
	var configuration bytes.Buffer
	check(toml.NewEncoder(&configuration).Encode(map[string]any{"page_runtime": profile}))
	check(os.WriteFile(filepath.Join(root, "vault", "config.toml"), configuration.Bytes(), 0600))
}
