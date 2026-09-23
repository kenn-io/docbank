package loadfile

import "strings"

// PackageFieldSensitive classifies a confirmed load-file field for public
// disclosure. Unknown columns stay private even when their sender header looks
// harmless; an explicit sensitive mapping always wins.
func PackageFieldSensitive(canonicalKey string, explicitlySensitive bool) bool {
	if explicitlySensitive {
		return true
	}
	switch canonicalKey {
	case "loadfile.label.begin", "loadfile.label.end", "loadfile.label.set",
		"loadfile.document.name", "loadfile.document.kind", "loadfile.document.description":
		return false
	}
	return !strings.HasPrefix(canonicalKey, "loadfile.date.") &&
		!strings.HasPrefix(canonicalKey, "loadfile.time.")
}
