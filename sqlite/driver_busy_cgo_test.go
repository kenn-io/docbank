//go:build cgo

package sqlite_test

import (
	"testing"

	"go.kenn.io/docbank/sqlite/mattn"
)

func TestMattnNoBusyWait(t *testing.T) {
	assertNoBusyWait(t, mattn.Driver{})
}
