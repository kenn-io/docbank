package providerutil

import (
	"errors"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/formatdetect"
)

// DecryptUpload prepares an in-memory provider copy using local inspection evidence.
func DecryptUpload(provider Provider, upload document.AuthorizedUpload, source []byte, maxBytes int64) ([]byte, string, error) {
	password, encrypted := document.UploadPDFPassword(upload)
	if !encrypted {
		return source, "", nil
	}
	unlocked, err := formatdetect.DecryptPDF(source, password, maxBytes)
	if err != nil {
		message := "PDF decryption failed"
		if errors.Is(err, formatdetect.ErrPDFExpandedBytes) {
			message = "decrypted PDF exceeds the byte limit"
		}
		return nil, "", provider.Classified(document.RenditionErrorPolicyRejected, message, err)
	}
	return unlocked, SHA256Hex(unlocked), nil
}
