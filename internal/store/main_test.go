package store

import (
	"os"
	"testing"

	"go.kenn.io/docbank/internal/sqlitetest"
)

func TestMain(m *testing.M) {
	sqlitetest.DisableSync.Store(true)
	os.Exit(m.Run())
}
