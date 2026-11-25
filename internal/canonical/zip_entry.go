package canonical

import "archive/zip"

// ZipStoredFile excludes entry types and ZIP features absent from these contracts.
func ZipStoredFile(entry *zip.File) bool {
	return entry.Mode().IsRegular() && entry.Method == zip.Store && entry.Flags&1 == 0 &&
		len(entry.Extra) == 0 && entry.Comment == "" && entry.CompressedSize64 == entry.UncompressedSize64
}
