package bundle

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/filename"
)

// DownloadRequest affects Content-Disposition only, never archive paths.
type DownloadRequest struct {
	Basename string `json:"basename,omitzero"`
}

func DownloadBasename(name string) (string, error) {
	if name == "" {
		return "docbank-bundle.zip", nil
	}
	invalid := func() (string, error) { return "", errors.New("invalid ZIP download basename") }
	if !utf8.ValidString(name) || len(name) > 180 || len(name) <= 4 || !strings.HasSuffix(name, ".zip") || strings.TrimSpace(name) != name || strings.ContainsAny(name, `/\:<>"|?*`) {
		return invalid()
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return invalid()
		}
	}
	stem := strings.ToUpper(strings.TrimRight(strings.SplitN(name, ".", 2)[0], " ."))
	if stem == "" || filename.IsWindowsDeviceName(stem) {
		return invalid()
	}
	return name, nil
}
