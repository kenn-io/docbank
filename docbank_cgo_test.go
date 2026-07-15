//go:build cgo

package docbank

import (
	"testing"

	"go.kenn.io/docbank/sqlite/mattn"
)

func TestEmbeddedVaultWithCGOSQLite(t *testing.T) {
	testEmbeddedVaultLifecycle(t, mattn.Driver{})
}
