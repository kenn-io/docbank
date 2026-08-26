//go:build !darwin

package ingest

import "io/fs"

func isCloudPlaceholder(fs.FileInfo) bool { return false }

func placeholderReadHint(error) string { return "" }
