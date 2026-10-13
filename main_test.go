package docbank_test

import (
	"os"
	"testing"

	"go.kenn.io/docbank/internal/packagetest"
)

func TestMain(m *testing.M) {
	os.Exit(packagetest.RunWithLockRegistry(m))
}
