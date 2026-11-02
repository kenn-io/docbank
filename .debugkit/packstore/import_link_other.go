//go:build !unix && !windows

package packstore

func isImportLinkUnsupported(error) bool { return false }
