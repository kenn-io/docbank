//go:build !windows

package mcp

import "os"

func createPrivateExportArchiveSpoolAt(parent string) (*exportArchiveSpool, error) {
	file, err := os.CreateTemp(parent, "docbank-mcp-export-")
	if err != nil {
		return nil, err
	}
	return &exportArchiveSpool{file: file, path: file.Name()}, nil
}
