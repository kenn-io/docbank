package api_test

import (
	"os"
	"testing"

	"go.kenn.io/docbank/internal/packagetest"
	"go.kenn.io/docbank/internal/pdfiumtest"
	"go.kenn.io/docbank/internal/sqlitetest"
)

func TestMain(m *testing.M) {
	sqlitetest.DisableSync.Store(true)
	pdfiumtest.ShareCompiledCode()
	os.Exit(packagetest.RunShardsWithLockRegistry(m))
}
