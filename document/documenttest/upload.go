// Package documenttest provides doubles for the document package's provider tests.
package documenttest

import (
	"io"

	"go.kenn.io/docbank/document"
)

// Upload serves a reader as an authorized upload with fixed metadata.
type Upload struct {
	io.ReadCloser

	Meta document.AuthorizedUploadMetadata
}

// Metadata returns the fixed upload metadata.
func (upload *Upload) Metadata() document.AuthorizedUploadMetadata { return upload.Meta }
