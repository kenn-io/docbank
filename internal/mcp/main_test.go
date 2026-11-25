package mcp

import (
	"os"
	"testing"

	"go.kenn.io/docbank/internal/packagetest"
	"go.kenn.io/docbank/internal/pdfproduction"
)

// The tests here run serially, so spread them over several processes.
func TestMain(m *testing.M) {
	pdfproduction.ShareCompiledCodeFromEnv()
	os.Exit(packagetest.RunShards(m))
}
