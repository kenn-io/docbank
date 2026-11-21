package mcp

import (
	"os"
	"testing"

	"go.kenn.io/docbank/internal/packagetest"
	"go.kenn.io/docbank/internal/pdfiumtest"
	"go.kenn.io/docbank/internal/sqlitetest"
)

// The tests here run serially, so spread them over several processes.
func TestMain(m *testing.M) {
	sqlitetest.DisableSync.Store(true)
	pdfiumtest.ShareCompiledCode()
	os.Exit(packagetest.RunShards(m))
}
