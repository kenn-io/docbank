package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--grandchild" {
		grandchild()
	}
	if len(os.Args) != 10 {
		fail("unexpected arguments")
	}
	if os.Args[1] != "--headless" || os.Args[2] != "--norestore" || os.Args[3] != "--nolockcheck" ||
		!strings.HasPrefix(os.Args[4], "-env:UserInstallation=file:///") || os.Args[5] != "--convert-to" ||
		os.Args[6] != "pdf:writer_pdf_Export" || os.Args[7] != "--outdir" {
		fail("unexpected arguments")
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		fail("read working directory")
	}
	if filepath.Clean(os.Args[8]) != filepath.Join(workingDirectory, "output") ||
		filepath.Clean(os.Args[9]) != filepath.Join(workingDirectory, "input", "source.docx") {
		fail("unexpected output paths")
	}
	for _, directory := range []string{"input", "output", "profile", "home", "tmp"} {
		info, statErr := os.Stat(filepath.Join(workingDirectory, directory))
		if statErr != nil || !info.IsDir() {
			fail("private conversion directory is missing")
		}
	}
	if os.Getenv("DOCBANK_DOCXPDF_AMBIENT_SECRET") != "" || os.Getenv("MISTRAL_API_KEY") != "" {
		fail("ambient credential reached child")
	}
	if os.Getenv("LANG") != "C.UTF-8" || os.Getenv("LC_ALL") != "C.UTF-8" || os.Getenv("TZ") != "UTC" {
		fail("controlled environment changed")
	}
	if runtime.GOOS == "windows" {
		for _, name := range []string{"TEMP", "TMP"} {
			if filepath.Clean(os.Getenv(name)) != filepath.Join(workingDirectory, "tmp") {
				fail("controlled environment changed")
			}
		}
		for _, name := range []string{"USERPROFILE", "APPDATA", "LOCALAPPDATA"} {
			if filepath.Clean(os.Getenv(name)) != filepath.Join(workingDirectory, "home") {
				fail("controlled environment changed")
			}
		}
	} else if os.Getenv("HOME") != filepath.Join(workingDirectory, "home") ||
		os.Getenv("TMPDIR") != filepath.Join(workingDirectory, "tmp") || os.Getenv("PATH") != "/usr/bin:/bin" {
		fail("controlled environment changed")
	}
	if _, err := os.Stat(os.Args[9]); err != nil {
		fail("input file is missing")
	}
	mode := strings.TrimSuffix(filepath.Base(os.Args[0]), filepath.Ext(os.Args[0]))
	mode = strings.TrimPrefix(mode, "renderer-")
	switch mode {
	case "fail":
		_, _ = io.WriteString(os.Stderr, "synthetic renderer failure")
		os.Exit(7)
	case "none":
		return
	case "notpdf":
		writeOutput(workingDirectory, []byte("not a PDF"))
	case "zero":
		writeOutput(workingDirectory, []byte("%PDF-1.4\n%%EOF\n"))
	case "dir":
		if err := os.Mkdir(filepath.Join(workingDirectory, "output", "source.pdf"), 0o700); err != nil {
			fail("create output directory")
		}
	case "size":
		writeOutput(workingDirectory, bytes.Repeat([]byte("x"), 2048))
	case "pages":
		writeOutput(workingDirectory, syntheticPDF(4))
	case "noise":
		_, _ = io.WriteString(os.Stdout, "synthetic stdout noise")
		_, _ = io.WriteString(os.Stderr, "synthetic stderr noise")
		writeOutput(workingDirectory, syntheticPDF(3))
	case "hang":
		markStarted(workingDirectory)
		for {
			time.Sleep(time.Hour)
		}
	case "tree":
		markStarted(workingDirectory)
		child := exec.Command(os.Args[0], "--grandchild") //nolint:gosec // the test helper starts its own fixed executable
		child.Dir = workingDirectory
		child.Stdout = io.Discard
		child.Stderr = io.Discard
		if err := child.Start(); err != nil {
			fail("start grandchild")
		}
		for {
			time.Sleep(time.Hour)
		}
	case "restart":
		if _, err := os.Stat(filepath.Join(workingDirectory, "restart-seen")); os.IsNotExist(err) {
			if err := os.WriteFile(filepath.Join(workingDirectory, "restart-seen"), []byte("seen"), 0o600); err != nil {
				fail("write restart marker")
			}
			os.Exit(81)
		}
		writeOutput(workingDirectory, syntheticPDF(3))
	case "restart-always":
		os.Exit(81)
	default:
		writeOutput(workingDirectory, syntheticPDF(3))
	}
}

func grandchild() {
	workingDirectory, err := os.Getwd()
	if err != nil {
		os.Exit(7)
	}
	file, err := os.OpenFile(filepath.Join(workingDirectory, "grandchild-held"), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		os.Exit(7)
	}
	defer func() { _ = file.Close() }()
	markStarted(workingDirectory)
	for {
		time.Sleep(time.Hour)
	}
}

func markStarted(directory string) {
	if err := os.WriteFile(filepath.Join(directory, "started"), []byte("started"), 0o600); err != nil {
		fail("write start marker")
	}
}

func writeOutput(directory string, data []byte) {
	if err := os.WriteFile(filepath.Join(directory, "output", "source.pdf"), data, 0o600); err != nil {
		fail("write output")
	}
}

func syntheticPDF(pageCount int) []byte {
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", pageReferences(pageCount), pageCount),
	}
	for range pageCount {
		objects = append(objects, "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>")
	}
	var output bytes.Buffer
	_, _ = output.WriteString("%PDF-1.4\n%synthetic\n")
	offsets := make([]int, len(objects))
	for index, object := range objects {
		offsets[index] = output.Len()
		_, _ = fmt.Fprintf(&output, "%d 0 obj\n%s\nendobj\n", index+1, object)
	}
	xref := output.Len()
	_, _ = fmt.Fprintf(&output, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		_, _ = fmt.Fprintf(&output, "%010d 00000 n \n", offset)
	}
	_, _ = fmt.Fprintf(&output, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return output.Bytes()
}

func pageReferences(pageCount int) string {
	references := make([]string, pageCount)
	for index := range pageCount {
		references[index] = fmt.Sprintf("%d 0 R", index+3)
	}
	return strings.Join(references, " ")
}

func fail(message string) {
	_, _ = fmt.Fprintln(os.Stderr, message)
	os.Exit(7)
}
