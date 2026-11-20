package pgschema

import (
	"os"
	"testing"

	"go.kenn.io/docbank/internal/pgtest"
)

func TestMain(m *testing.M) {
	os.Exit(pgtest.Finish(m.Run()))
}
