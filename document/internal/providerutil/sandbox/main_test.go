package sandbox

import (
	"os"
	"testing"

	"go.kenn.io/docbank/internal/packagetest"
)

// The tests here run serially, so spread them over several processes.
func TestMain(m *testing.M) {
	os.Exit(packagetest.RunShards(m))
}
