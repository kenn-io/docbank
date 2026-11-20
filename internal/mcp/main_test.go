package mcp

import (
	"os"
	"testing"

	"go.kenn.io/docbank/internal/packagetest"
	"go.kenn.io/docbank/internal/pdfiumtest"
)

// The tests here run serially, so spread them over several processes.
func TestMain(m *testing.M) {
	pdfiumtest.ShareCompiledCode()
	os.Exit(packagetest.RunShards(m))
}
