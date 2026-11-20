package backupapp

import (
	"os"
	"testing"

	"go.kenn.io/docbank/internal/packagetest"
	"go.kenn.io/docbank/internal/pgstoretest"
)

// The tests here run serially, so spread them over several processes.
func TestMain(m *testing.M) {
	os.Exit(pgstoretest.Finish(packagetest.RunShards(m)))
}
