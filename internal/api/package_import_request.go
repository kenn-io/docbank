package api

import (
	"crypto/sha256"
	"encoding/hex"

	"go.kenn.io/docbank/internal/canonical"
)

func packageImportRequestHash(request PackageImportRequest) (string, error) {
	request.OperationID = ""
	raw, err := canonical.Marshal(request)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}
