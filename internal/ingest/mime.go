package ingest

import (
	"mime"
	"path/filepath"
	"slices"
	"strings"

	"github.com/gabriel-vasile/mimetype"
)

// DetectMIME resolves a MIME type from the file bytes and file extension.
func DetectMIME(path string, head []byte) string {
	return detectMimeWithExtension(path, head, mime.TypeByExtension)
}

func detectMime(path string, head []byte) string {
	return DetectMIME(path, head)
}

func detectMimeWithExtension(
	path string,
	head []byte,
	byExtension func(string) string,
) string {
	extension := filepath.Ext(path)
	if strings.EqualFold(extension, ".eml") {
		return "message/rfc822"
	}

	detected := mimetype.Detect(head)
	if len(head) == 0 || detected.Is("application/octet-stream") {
		if strings.EqualFold(extension, ".raf") {
			return "image/x-fuji-raf"
		}
		return extensionMIME(extension, byExtension, detected.String())
	}

	if closedRefinement := closedMIMERefinement(detected, extension); closedRefinement != "" {
		return closedRefinement
	}
	if !extensionCanRefine(detected) {
		return detected.String()
	}

	if extensionMIME, ok := parsedExtensionMIME(extension, byExtension); ok &&
		compatibleMIME(detected, extensionMIME) {
		return extensionMIME
	}
	return detected.String()
}

func extensionMIME(
	extension string,
	byExtension func(string) string,
	fallback string,
) string {
	value, ok := parsedExtensionMIME(extension, byExtension)
	if !ok {
		return fallback
	}
	return value
}

func parsedExtensionMIME(
	extension string,
	byExtension func(string) string,
) (string, bool) {
	value := byExtension(extension)
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil || mediaType == "" {
		return "", false
	}
	return value, true
}

func compatibleMIME(detected *mimetype.MIME, extension string) bool {
	extensionMediaType, _, err := mime.ParseMediaType(extension)
	if err != nil {
		return false
	}
	extensionNode := mimetype.Lookup(extensionMediaType)
	if extensionNode == nil {
		return false
	}
	if sameMIMENode(detected, extensionNode) || descendantOf(extensionNode, detected) {
		return true
	}
	return textFamilyMIME(detected) && textFamilyMIME(extensionNode)
}

func sameMIMENode(left, right *mimetype.MIME) bool {
	return left.Is(right.String()) || right.Is(left.String())
}

func descendantOf(child, ancestor *mimetype.MIME) bool {
	if ancestor == nil {
		return false
	}
	for current := child; current != nil; current = current.Parent() {
		if sameMIMENode(current, ancestor) {
			return true
		}
	}
	return false
}

func textFamilyMIME(value *mimetype.MIME) bool {
	text := mimetype.Lookup("text/plain")
	return text != nil && descendantOf(value, text)
}

func extensionCanRefine(detected *mimetype.MIME) bool {
	if textFamilyMIME(detected) {
		return true
	}
	// These detector roots leave subtype selection to the filename.
	return slices.ContainsFunc([]string{
		"application/zip",
		"application/x-ole-storage",
		"application/ogg",
		"application/gzip",
		"video/mp4",
		"video/webm",
	}, detected.Is)
}

func closedMIMERefinement(
	detected *mimetype.MIME,
	extension string,
) string {
	switch strings.ToLower(extension) {
	case ".arw":
		if detected.Is("image/tiff") {
			return "image/x-sony-arw"
		}
	case ".dng":
		if detected.Is("image/tiff") {
			return "image/x-adobe-dng"
		}
	case ".cr2":
		if detected.Is("image/tiff") {
			return "image/x-canon-cr2"
		}
	case ".nef":
		if detected.Is("image/tiff") {
			return "image/x-nikon-nef"
		}
	case ".apng":
		if descendantOf(detected, mimetype.Lookup("image/png")) {
			return "image/apng"
		}
	case ".mka":
		if detected.Is("video/x-matroska") {
			return "audio/x-matroska"
		}
	case ".xmp":
		if descendantOf(detected, mimetype.Lookup("text/xml")) || textFamilyMIME(detected) {
			return "application/rdf+xml"
		}
	case ".go":
		if textFamilyMIME(detected) {
			return "text/x-go"
		}
	case ".rst":
		if textFamilyMIME(detected) {
			return "text/x-rst"
		}
	case ".yaml", ".yml":
		if textFamilyMIME(detected) {
			return "application/yaml"
		}
	case ".tex":
		if textFamilyMIME(detected) {
			return "application/x-tex"
		}
	case ".md", ".markdown":
		if textFamilyMIME(detected) {
			return "text/markdown"
		}
	}
	return ""
}
