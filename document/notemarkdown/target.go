package notemarkdown

import (
	"path"
	"strings"
	"unicode/utf8"
)

// PlanTarget classifies one syntax-decoded target without I/O. Fixed 4096-byte
// source/target/key bounds keep this context-free operation small. Invalid
// authored targets are explicit classifications; malformed API inputs and
// resource failures return errors.
func PlanTarget(sourcePath string, link Link) (TargetQuery, error) {
	if err := limitCount("target_bytes", len(sourcePath), maxTarget); err != nil {
		return TargetQuery{}, err
	}
	if !canonicalSourcePath(sourcePath) {
		return TargetQuery{}, ErrInvalidInput
	}
	if link.Form != "markdown" && link.Form != "markdown_reference" && link.Form != "wiki" {
		return TargetQuery{}, ErrInvalidInput
	}
	target := link.Target
	if err := limitCount("target_bytes", len(target), maxTarget); err != nil {
		return TargetQuery{}, err
	}
	invalid := TargetQuery{Kind: "invalid"}
	if !utf8.ValidString(target) || strings.ContainsRune(target, 0) {
		return invalid, nil
	}
	if drivePath(target) || strings.HasPrefix(target, `\\`) {
		return invalid, nil
	}
	if strings.HasPrefix(target, "//") || uriScheme(target) {
		return TargetQuery{Kind: "external"}, nil
	}
	if link.Form != "wiki" && strings.HasPrefix(target, "/") {
		return invalid, nil
	}
	// Split authored URI syntax before decoding. Encoded '?' and '#' are
	// filename bytes, and an encoded '%' never starts a second decoding pass.
	pathPart, fragment := target, ""
	if hash := strings.IndexByte(pathPart, '#'); hash >= 0 {
		pathPart, fragment = pathPart[:hash], pathPart[hash+1:]
	}
	hasQuery := false
	if query := strings.IndexByte(pathPart, '?'); query >= 0 {
		pathPart = pathPart[:query]
		hasQuery = true
	}
	decoded, ok := decodeURIComponent(pathPart)
	if !ok {
		return invalid, nil
	}
	fragment, ok = decodeURIComponent(fragment)
	if !ok {
		return invalid, nil
	}
	if hasQuery {
		return TargetQuery{Kind: "unsupported"}, nil
	}
	if strings.HasPrefix(fragment, "^") {
		return TargetQuery{Kind: "unsupported", Fragment: fragment}, nil
	}
	if drivePath(decoded) || strings.HasPrefix(decoded, `\\`) {
		return invalid, nil
	}
	if decoded == "" {
		return TargetQuery{Kind: "note", Fragment: fragment, Keys: []LookupKey{{Kind: "path", Value: sourcePath}}}, nil
	}
	root := strings.HasPrefix(decoded, "/")
	if root && link.Form != "wiki" {
		return invalid, nil
	}
	// Bare-name fallback is a wiki-only convenience. Explicit paths and
	// explicit extensions never gain another spelling or normalization key.
	if last := path.Base(decoded); last == "." || last == ".." || strings.HasSuffix(decoded, "/") {
		return invalid, nil
	}
	ext := path.Ext(decoded)
	bare := link.Form == "wiki" && ext == "" && !strings.Contains(decoded, "/") && decoded != "." && decoded != ".."
	lookup := decoded
	if link.Form == "wiki" && ext == "" {
		lookup += ".md"
	}
	parent := ""
	if !root {
		parent = path.Dir(sourcePath)
		if parent == "." {
			parent = ""
		}
	}
	key, ok := confinedPath(parent, lookup)
	if !ok {
		return invalid, nil
	}
	if err := limitCount("target_bytes", len(key), maxTarget); err != nil {
		return TargetQuery{}, err
	}
	if !canonicalSourcePath(key) {
		return invalid, nil
	}
	if ext != "" && !markdownExtension(ext) {
		return TargetQuery{Kind: "attachment", Fragment: fragment}, nil
	}
	keys := []LookupKey{{Kind: "path", Value: key}}
	if bare {
		keys = append(keys, LookupKey{Kind: "basename", Value: decoded}, LookupKey{Kind: "alias", Value: decoded})
	}
	return TargetQuery{Kind: "note", Fragment: fragment, Keys: keys}, nil
}

func canonicalSourcePath(value string) bool {
	if value == "" || !utf8.ValidString(value) || strings.ContainsRune(value, 0) || strings.HasPrefix(value, "/") || strings.HasPrefix(value, `\\`) || drivePath(value) {
		return false
	}
	for component := range strings.SplitSeq(value, "/") {
		if component == "" || component == "." || component == ".." {
			return false
		}
	}
	return true
}

func drivePath(value string) bool {
	return len(value) >= 2 && ((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z')) && value[1] == ':'
}

func uriScheme(value string) bool {
	if len(value) == 0 || !asciiLetter(value[0]) {
		return false
	}
	for i := 1; i < len(value); i++ {
		c := value[i]
		if c == ':' {
			return true
		}
		if !asciiLetter(c) && (c < '0' || c > '9') && c != '+' && c != '-' && c != '.' {
			return false
		}
	}
	return false
}

func asciiLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

func decodeURIComponent(value string) (string, bool) {
	var out strings.Builder
	out.Grow(len(value))
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c == '%' {
			if i+2 >= len(value) {
				return "", false
			}
			hi, ok := hexNibble(value[i+1])
			if !ok {
				return "", false
			}
			lo, ok := hexNibble(value[i+2])
			if !ok {
				return "", false
			}
			c = hi<<4 | lo
			i += 2
			if c == '/' || c == '\\' || c == 0 {
				return "", false
			}
		}
		out.WriteByte(c)
	}
	result := out.String()
	return result, utf8.ValidString(result) && !strings.ContainsRune(result, 0)
}

func hexNibble(c byte) (byte, bool) {
	if c >= '0' && c <= '9' {
		return c - '0', true
	}
	if c >= 'a' && c <= 'f' {
		return c - 'a' + 10, true
	}
	if c >= 'A' && c <= 'F' {
		return c - 'A' + 10, true
	}
	return 0, false
}

func confinedPath(parent, target string) (string, bool) {
	var components []string
	if parent != "" {
		components = strings.Split(parent, "/")
	}
	for part := range strings.SplitSeq(strings.TrimPrefix(target, "/"), "/") {
		switch part {
		case "", ".":
		case "..":
			if len(components) == 0 {
				return "", false
			}
			components = components[:len(components)-1]
		default:
			components = append(components, part)
		}
	}
	if len(components) == 0 {
		return "", false
	}
	return strings.Join(components, "/"), true
}

func markdownExtension(extension string) bool {
	return strings.EqualFold(extension, ".md") || strings.EqualFold(extension, ".markdown")
}
