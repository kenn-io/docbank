// Package zipcheck shares entry rules for the fixed Store archives Docbank publishes.
package zipcheck

import "archive/zip"

// StoredFile excludes entry types and ZIP features absent from these contracts.
func StoredFile(entry *zip.File) bool {
	return entry.Mode().IsRegular() && entry.Method == zip.Store && entry.Flags&1 == 0 &&
		len(entry.Extra) == 0 && entry.Comment == "" && entry.CompressedSize64 == entry.UncompressedSize64
}
