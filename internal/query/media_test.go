package query

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/docbank/document"
)

func TestClassifyMediaCoversEveryDeclaredFamily(t *testing.T) {
	tests := []struct {
		family, mediaType, filename string
	}{
		{"email", "message/rfc822", "synthetic.eml"},
		{"document", "application/pdf", "synthetic.pdf"},
		{"spreadsheet", "text/csv", "synthetic.csv"},
		{"presentation", "application/vnd.ms-powerpoint", "synthetic.ppt"},
		{"image", "image/vnd.synthetic", "synthetic.bin"},
		{"audio_video", "audio/vnd.synthetic", "synthetic.bin"},
		{"text", "text/plain", "synthetic.txt"},
		{"source_code", "text/x-go", "synthetic.go"},
		{"web", "text/html", "synthetic.html"},
		{"calendar", "text/calendar", "synthetic.ics"},
		{"archive", "application/zip", "synthetic.zip"},
		{"cad", "image/vnd.dwg", "synthetic.dwg"},
		{"unknown", "application/vnd.synthetic-unknown", "synthetic.pdf"},
	}
	for _, testCase := range tests {
		t.Run(testCase.family, func(t *testing.T) {
			assert.Equal(t, testCase.family, ClassifyMedia(testCase.mediaType, testCase.filename))
		})
	}
}

func TestClassifyMediaUsesEveryDocumentOwnedMapping(t *testing.T) {
	providerFamilies := map[string]string{
		"pdf": "document", "word": "document", "ebook": "document", "mail": "email",
		"source": "source_code", "structured": "text", "spreadsheet": "spreadsheet",
		"presentation": "presentation", "text": "text",
	}
	for _, metadata := range document.FormatMetadataCatalog() {
		assert.Equal(t, metadata.QueryFamily, ClassifyMedia(metadata.MediaType, "synthetic.bin"), metadata.ID)
		for _, extension := range metadata.Extensions {
			assert.Equal(t, metadata.QueryFamily, ClassifyMedia("application/octet-stream", "synthetic."+extension), metadata.ID)
		}
		if metadata.Provider {
			assert.Equal(t, providerFamilies[metadata.Family], metadata.QueryFamily, metadata.ID)
		}
	}
}

func TestClassifyMediaUsesDeclaredMIMEBeforeExtension(t *testing.T) {
	for _, testCase := range loadIdentityFixture(t).MediaCases {
		t.Run(testCase.Name, func(t *testing.T) {
			assert.Equal(t, testCase.Family, ClassifyMedia(testCase.MediaType, testCase.Filename))
		})
	}
	assert.Equal(t, "image", ClassifyMedia(" IMAGE/PNG ; name=synthetic ", "message.eml"))
	assert.Equal(t, "unknown", ClassifyMedia("application/vnd.synthetic-unknown", "report.pdf"))
	assert.Equal(t, "document", ClassifyMedia("application/octet-stream", "REPORT.PDF"))
	assert.Equal(t, "image", ClassifyMedia("application/octet-stream", "scan.PNG"))
	assert.Equal(t, "audio_video", ClassifyMedia("application/octet-stream", "recording.MP3"))
	assert.Equal(t, "source_code", ClassifyMedia("", "main.GO"))
	assert.Equal(t, "unknown", ClassifyMedia("not a mime", "report.pdf"))
}
