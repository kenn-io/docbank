//go:build !windows

package mcp

import "os"

func createPrivateReportSpoolAt(parent string) (reportSpool, error) {
	file, err := os.CreateTemp(parent, "docbank-mcp-report-")
	if err != nil {
		return reportSpool{}, err
	}
	return reportSpool{file: file, path: file.Name()}, nil
}
