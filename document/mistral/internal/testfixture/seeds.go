package testfixture

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// CompoundDocument builds a synthetic compound document.
func CompoundDocument(streamNames ...string) []byte {
	const (
		freeSector = uint32(0xffffffff)
		endOfChain = uint32(0xfffffffe)
		fatSector  = uint32(0xfffffffd)
	)
	content := make([]byte, 3*512)
	header := content[:512]
	copy(header, []byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1})
	binary.LittleEndian.PutUint16(header[26:28], 3)
	binary.LittleEndian.PutUint16(header[28:30], 0xfffe)
	binary.LittleEndian.PutUint16(header[30:32], 9)
	binary.LittleEndian.PutUint16(header[32:34], 6)
	binary.LittleEndian.PutUint32(header[44:48], 1)
	binary.LittleEndian.PutUint32(header[48:52], 1)
	binary.LittleEndian.PutUint32(header[56:60], 4096)
	binary.LittleEndian.PutUint32(header[60:64], endOfChain)
	binary.LittleEndian.PutUint32(header[68:72], endOfChain)
	for offset := 76; offset < 512; offset += 4 {
		binary.LittleEndian.PutUint32(header[offset:offset+4], freeSector)
	}
	binary.LittleEndian.PutUint32(header[76:80], 0)

	fat := content[512:1024]
	for offset := 0; offset < len(fat); offset += 4 {
		binary.LittleEndian.PutUint32(fat[offset:offset+4], freeSector)
	}
	binary.LittleEndian.PutUint32(fat[0:4], fatSector)
	binary.LittleEndian.PutUint32(fat[4:8], endOfChain)

	directory := content[1024:]
	writeCompoundDirectoryEntry(directory[:128], "Root Entry", 5)
	if len(streamNames) > 0 {
		binary.LittleEndian.PutUint32(directory[76:80], 1)
	}
	for i, name := range streamNames {
		offset := (i + 1) * 128
		if offset+128 > len(directory) {
			panic("too many synthetic compound streams")
		}
		writeCompoundDirectoryEntry(directory[offset:offset+128], name, 2)
		if i+1 < len(streamNames) {
			binary.LittleEndian.PutUint32(directory[offset+72:offset+76], uint32(i+2))
		}
	}
	return content
}

func writeCompoundDirectoryEntry(entry []byte, name string, entryType byte) {
	encoded := make([]byte, 0, (len(name)+1)*2)
	for _, character := range name {
		if character >= 0x10000 {
			panic("invalid synthetic compound name")
		}
		encoded = binary.LittleEndian.AppendUint16(encoded, uint16(character)) //nolint:gosec // The name characters fit in one UTF-16 code unit, checked above.
	}
	encoded = binary.LittleEndian.AppendUint16(encoded, 0)
	if len(encoded) > 64 {
		panic("synthetic compound name too long")
	}
	copy(entry, encoded)
	binary.LittleEndian.PutUint16(entry[64:66], uint16(len(encoded))) //nolint:gosec // The encoded name is at most 64 bytes, checked above.
	entry[66] = entryType
	binary.LittleEndian.PutUint32(entry[68:72], uint32(0xffffffff))
	binary.LittleEndian.PutUint32(entry[72:76], uint32(0xffffffff))
	binary.LittleEndian.PutUint32(entry[76:80], uint32(0xffffffff))
}

// DocumentZIP builds a synthetic document archive.
func DocumentZIP(entries map[string]string) ([]byte, error) {
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	for name, value := range entries {
		entry, err := writer.Create(name)
		if err != nil {
			return nil, fmt.Errorf("create synthetic ZIP entry: %w", err)
		}
		if _, err := io.WriteString(entry, value); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("close synthetic ZIP: %w", err)
	}
	return output.Bytes(), nil
}

// WriteNativeSeeds writes synthetic native probe seeds into an existing directory.
func WriteNativeSeeds(directory string) error {
	numbers, err := DocumentZIP(map[string]string{"Index/Tables/Table.iwa": "synthetic"})
	if err != nil {
		return err
	}
	for name, content := range map[string][]byte{
		"doc": CompoundDocument("WordDocument"), "ppt": CompoundDocument("PowerPoint Document"),
		"xls": CompoundDocument("Workbook"), "msg": CompoundDocument("__properties_version1.0"), "numbers": numbers,
	} {
		if err := os.WriteFile(filepath.Join(directory, name), content, 0o600); err != nil {
			return err
		}
	}
	return nil
}
