package processing

import (
	"archive/zip"
	"bytes"
	"context"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/docx"
)

func (c *metadataCollector) extractDOCXFingerprint(ctx context.Context, data []byte) error {
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil //nolint:nilerr // The OOXML property reader reports malformed containers.
	}
	for _, entry := range archive.File {
		if entry.Name != "word/document.xml" {
			continue
		}
		fingerprint, err := docx.Fingerprint(ctx, data)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			c.warn("package_fingerprint_unavailable", "office.package", "zip entries", "DOCX package could not be completely verified within inspection limits")
			return nil //nolint:nilerr // Invalid source bytes are durable warnings, not retryable failures.
		}
		c.string(document.DOCXPackageFingerprintKey, "office.package", "zip entries", fingerprint, false)
		return nil
	}
	return nil
}
