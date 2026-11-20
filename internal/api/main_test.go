package api_test

import (
	"os"
	"testing"

	"go.kenn.io/docbank/internal/packagetest"
	"go.kenn.io/docbank/internal/pdfiumtest"
)

func TestMain(m *testing.M) {
	pdfiumtest.ShareCompiledCode()
	os.Exit(packagetest.RunShardsWithLockRegistry(m))
}
