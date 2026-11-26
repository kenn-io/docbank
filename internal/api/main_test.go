package api_test

import (
	"os"
	"testing"

	"go.kenn.io/docbank/internal/packagetest"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/pgstoretest"
)

func TestMain(m *testing.M) {
	pdfproduction.ShareCompiledCodeFromEnv()
	os.Exit(pgstoretest.Finish(packagetest.RunShardsWithLockRegistry(m)))
}
