package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	protocol        = "docbank-pymupdf/v1"
	runtimeIdentity = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

func main() {
	if len(os.Args) != 3 || os.Args[1] != "--protocol" || os.Args[2] != protocol {
		fail("unexpected arguments")
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		fail("read working directory")
	}
	executable, err := os.Executable()
	if err != nil {
		fail("read executable path")
	}
	workingInfo, err := os.Stat(workingDirectory)
	if err != nil {
		fail("stat working directory")
	}
	executableDirectoryInfo, err := os.Stat(filepath.Dir(executable))
	if err != nil || !os.SameFile(workingInfo, executableDirectoryInfo) {
		fail("executable did not start in its private directory")
	}
	// The operating system may synthesize bookkeeping variables that Cmd.Env
	// cannot control. Verify the security boundary directly: ambient caller
	// state is absent and every provider-controlled value is exact.
	if os.Getenv("DOCBANK_PYMUPDF_AMBIENT_SECRET") != "" {
		fail("ambient environment reached child")
	}
	for name, expected := range map[string]string{
		"LANG": "C.UTF-8", "LC_ALL": "C.UTF-8", "TZ": "UTC", "PYTHONHASHSEED": "0",
		"PYTHONNOUSERSITE": "1", "PYTHONDONTWRITEBYTECODE": "1",
	} {
		if os.Getenv(name) != expected {
			fail("controlled environment changed")
		}
	}
	source, err := io.ReadAll(os.Stdin)
	if err != nil {
		fail("read stdin")
	}
	digest := sha256.Sum256(source)
	mode := strings.TrimSuffix(filepath.Base(os.Args[0]), filepath.Ext(os.Args[0]))
	mode = strings.TrimPrefix(mode, "renderer-")
	if mode == "failure" {
		_, _ = fmt.Fprintln(os.Stderr, "private-stderr-token")
		os.Exit(7)
	}
	if mode == "wait" {
		time.Sleep(10 * time.Second)
	}
	if mode == "unbounded-output" {
		chunk := []byte(strings.Repeat("x", 32<<10))
		for {
			if _, writeErr := os.Stdout.Write(chunk); writeErr != nil {
				time.Sleep(10 * time.Second)
			}
		}
	}
	if mode == "malformed" {
		_, _ = io.WriteString(os.Stdout, "{")
		return
	}
	if mode == "oversized" {
		_, _ = io.WriteString(os.Stdout, strings.Repeat("x", 2048))
		return
	}
	type outputPage struct {
		Number      int    `json:"number"`
		Text        string `json:"text"`
		EmptyReason string `json:"empty_reason,omitempty"`
	}
	response := struct {
		ContractVersion string       `json:"contract_version"`
		RuntimeIdentity string       `json:"runtime_identity"`
		SourceSHA256    string       `json:"source_sha256"`
		SourceBytes     int64        `json:"source_bytes"`
		Complete        bool         `json:"complete"`
		PageCount       int          `json:"page_count"`
		Pages           []outputPage `json:"pages"`
	}{
		ContractVersion: protocol, RuntimeIdentity: runtimeIdentity,
		SourceSHA256: hex.EncodeToString(digest[:]), SourceBytes: int64(len(source)),
		Complete: true, PageCount: 2,
	}
	response.Pages = append(response.Pages,
		outputPage{Number: 1, Text: "first page"},
		outputPage{Number: 2, Text: "second page"},
	)
	if mode == "many-pages" {
		const pages = 20_000
		response.PageCount = pages
		response.Pages = make([]outputPage, pages)
		for index := range pages {
			response.Pages[index] = outputPage{Number: index + 1, Text: "synthetic page text"}
		}
	}
	switch mode {
	case "version-drift":
		response.ContractVersion = "docbank-pymupdf/v2"
	case "runtime-drift":
		response.RuntimeIdentity = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	case "source-hash-drift":
		response.SourceSHA256 = strings.Repeat("b", 64)
	case "source-size-drift":
		response.SourceBytes++
	case "partial":
		response.Complete = false
	case "page-count-drift":
		response.PageCount++
	case "gap":
		response.Pages[1].Number = 3
	case "duplicate":
		response.Pages[1].Number = 1
	case "empty-unexplained":
		response.Pages[0].Text = ""
	case "empty-explained":
		response.Pages[0].Text = ""
		response.Pages[0].EmptyReason = "blank page"
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		fail("encode stdout")
	}
	if mode == "unknown-field" {
		encoded = append(encoded[:len(encoded)-1], []byte(`,"unexpected":true}`)...)
	}
	if _, err := os.Stdout.Write(encoded); err != nil {
		fail("write stdout")
	}
}

func fail(message string) {
	_, _ = fmt.Fprintln(os.Stderr, message)
	os.Exit(7)
}
