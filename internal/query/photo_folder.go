package query

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"strings"
)

// PhotoFolderKey names the import folder that bounds a burst. Push and watch
// sources keep their own namespaces, so equal paths from different sources do
// not join. Provenance can come from a Windows backup: absolute drive and UNC
// paths use portable separators, while POSIX paths preserve backslashes and Unicode bytes. Both
// SQLite drivers call this function when they derive photo_timeline.folder_key.
// Caller-supplied references stay opaque and cannot identify import folders.
func PhotoFolderKey(sourceKind, sourceDesc, originalPath string) (string, bool) {
	if originalPath == "" || IsEmbeddedSourceKind(sourceKind) {
		return "", false
	}
	portable := strings.ReplaceAll(originalPath, `\`, "/")
	drive := len(portable) >= 3 && (portable[0] >= 'A' && portable[0] <= 'Z' || portable[0] >= 'a' && portable[0] <= 'z') && portable[1:3] == ":/"
	unc := (strings.HasPrefix(originalPath, `\\`) || strings.HasPrefix(originalPath, "//")) && len(strings.Split(strings.TrimPrefix(portable, "//"), "/")) >= 3
	folder := path.Dir(originalPath)
	if drive || unc {
		folder = "windows:" + path.Dir(portable)
	}
	if folder == "." {
		return "", false
	}
	if sourceKind == "push" || sourceKind == "watch" {
		folder = "source\x00" + sourceKind + "\x00" + sourceDesc + "\x00" + folder
	}
	digest := sha256.Sum256([]byte(folder))
	return hex.EncodeToString(digest[:]), true
}

// IsEmbeddedSourceKind reports whether provenance was supplied by an embedded caller.
func IsEmbeddedSourceKind(kind string) bool {
	const prefix = "embedded:"
	return len(kind) >= len(prefix) && strings.EqualFold(kind[:len(prefix)], prefix)
}
