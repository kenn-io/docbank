package packagetest

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const maxIndependentPackageEntry = 512 << 20

// LoadFilePackage is an independently decoded export archive. It deliberately
// uses only standard-library ZIP, CSV, and JSON readers instead of Docbank's
// load-file parser or manifest verifier.
type LoadFilePackage struct {
	Entries     map[string][]byte
	LoadFile    string
	Rows        [][]string
	MappingJSON []byte
	Records     int
}

func ReadLoadFilePackage(data []byte) (LoadFilePackage, error) {
	var result LoadFilePackage
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return result, fmt.Errorf("open independent package ZIP: %w", err)
	}
	result.Entries = make(map[string][]byte, len(archive.File))
	for _, entry := range archive.File {
		if entry.FileInfo().IsDir() || entry.Name == "" || strings.Contains(entry.Name, `\`) ||
			path.IsAbs(entry.Name) || path.Clean(entry.Name) != entry.Name || strings.HasPrefix(entry.Name, "..") ||
			entry.UncompressedSize64 > maxIndependentPackageEntry || result.Entries[entry.Name] != nil {
			return LoadFilePackage{}, fmt.Errorf("unsafe or duplicate package entry %q", entry.Name)
		}
		reader, openErr := entry.Open()
		if openErr != nil {
			return LoadFilePackage{}, fmt.Errorf("open independent package entry %q: %w", entry.Name, openErr)
		}
		contents, readErr := io.ReadAll(io.LimitReader(reader, maxIndependentPackageEntry+1))
		if err := errors.Join(readErr, reader.Close()); err != nil || len(contents) > maxIndependentPackageEntry {
			return LoadFilePackage{}, errors.Join(err, errors.New("package entry exceeds independent reader bound"))
		}
		result.Entries[entry.Name] = contents
	}
	var receipt struct {
		LoadFile    string         `json:"load_file"`
		Mapping     jsontext.Value `json:"mapping"`
		RecordCount int            `json:"record_count"`
	}
	if err := json.Unmarshal(result.Entries["docbank/receipt.json"], &receipt); err != nil {
		return LoadFilePackage{}, fmt.Errorf("decode package receipt: %w", err)
	}
	if receipt.LoadFile == "" || len(receipt.Mapping) == 0 || receipt.RecordCount < 1 {
		return LoadFilePackage{}, errors.New("package receipt is incomplete")
	}
	loadFile := result.Entries[receipt.LoadFile]
	if loadFile == nil || !strings.HasSuffix(strings.ToUpper(receipt.LoadFile), ".CSV") {
		return LoadFilePackage{}, errors.New("independent reader requires a declared CSV load file")
	}
	rows, err := csv.NewReader(bytes.NewReader(loadFile)).ReadAll()
	if err != nil || len(rows) != receipt.RecordCount+1 {
		return LoadFilePackage{}, errors.Join(err, errors.New("CSV row count differs from package receipt"))
	}
	result.LoadFile = receipt.LoadFile
	result.Rows = rows
	result.MappingJSON = append([]byte(nil), receipt.Mapping...)
	result.Records = receipt.RecordCount
	return result, nil
}

func (p LoadFilePackage) Extract(root string) error {
	for name, data := range p.Entries {
		destination := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(destination, data, 0o600); err != nil {
			return err
		}
	}
	return nil
}
