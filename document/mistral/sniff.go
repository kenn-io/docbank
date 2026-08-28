package mistral

import (
	"io"

	"go.kenn.io/docbank/document/internal/formatdetect"
)

const (
	maxZIPEntries         = 10_000
	ooxmlContentTypesName = "[Content_Types].xml"
	compoundNoStream      = uint32(0xffffffff)
)

var compoundFileMagic = []byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1}

// DetectFormat validates a provider candidate from bounded bytes. Core media
// inspection and Mistral preparation share the same fail-closed detector.
func DetectFormat(reader io.ReaderAt, size int64, declaredMediaType string) (CandidateFormat, error) {
	return formatdetect.DetectFormat(reader, size, declaredMediaType)
}

func compoundDirectoryNames(reader io.ReaderAt, size int64) (map[string]bool, error) {
	return formatdetect.CompoundDirectoryNames(reader, size)
}

func candidateByMediaType(mediaType string) (CandidateFormat, bool) {
	for _, candidate := range candidateFormats {
		if candidate.MediaType == mediaType {
			return candidate, true
		}
	}
	return CandidateFormat{}, false
}
