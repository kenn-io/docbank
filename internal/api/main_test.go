package api_test

import (
	"os"
	"testing"

	"go.kenn.io/docbank/internal/packagetest"
	"go.kenn.io/docbank/internal/pdfproduction"
)

func TestMain(m *testing.M) {
	pdfproduction.ShareCompiledCodeFromEnv()
	os.Exit(packagetest.RunShardsWithLockRegistry(m))
}
