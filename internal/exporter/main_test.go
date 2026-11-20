package exporter_test

import (
	"os"
	"testing"

	"go.kenn.io/docbank/internal/pgstoretest"
)

func TestMain(m *testing.M) {
	os.Exit(pgstoretest.Finish(m.Run()))
}
