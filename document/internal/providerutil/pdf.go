package providerutil

import (
	"context"
	"errors"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/formatdetect"
)

// UnlockPDF prepares an in-memory provider copy using local inspection evidence.
func UnlockPDF(ctx context.Context, provider Provider, upload document.AuthorizedUpload, source []byte, maxBytes int64) ([]byte, string, error) {
	passwords := document.UploadPDFPasswords(upload)
	if len(passwords) == 0 {
		return source, "", nil
	}
	unlocked, _, err := formatdetect.UnlockPDF(ctx, source, passwords, maxBytes)
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		message := "PDF decryption failed"
		if errors.Is(err, formatdetect.ErrPDFEncrypted) {
			message = "PDF password required"
		}
		if errors.Is(err, formatdetect.ErrPDFExpandedBytes) {
			message = "decrypted PDF exceeds the byte limit"
		}
		return nil, "", provider.Classified(document.RenditionErrorPolicyRejected, message, err)
	}
	return unlocked, SHA256Hex(unlocked), nil
}
