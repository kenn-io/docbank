package store

import (
	"flag"
	"os"
	"testing"

	"go.kenn.io/docbank/internal/sqlitetest"
)

func TestMain(m *testing.M) {
	flag.Parse()
	if *reportBackendCoverage {
		printBackendCoverage()
		return
	}
	sqlitetest.DisableSync.Store(true)
	os.Exit(m.Run())
}
