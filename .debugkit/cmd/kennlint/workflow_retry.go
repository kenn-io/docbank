package main

import (
	"fmt"
	"io"
	"path/filepath"

	"go.kenn.io/kit/lint/workflowretry"
)

func runWorkflowRetries(paths []string, stdout io.Writer) (int, error) {
	if len(paths) == 0 {
		paths = []string{".github"}
	}
	return scanFiles(paths, []string{".yml", ".yaml"}, func(path string, src []byte) (int, error) {
		findings, err := workflowretry.Scan(src)
		if err != nil {
			return 0, err
		}
		for _, f := range findings {
			fmt.Fprintf(stdout, "%s:%d:%d: job %s, step %s: %s\n", filepath.ToSlash(path), f.Line, f.Column, f.Job, f.Step, f.Message)
		}
		return len(findings), nil
	})
}
