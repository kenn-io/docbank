package api_test

import (
	"os"
	"testing"

	"go.kenn.io/docbank/internal/packagetest"
	"go.kenn.io/kit/pack"
)

func TestMain(m *testing.M) {
	// Kit caches encoders globally; create their channels outside synctest bubbles.
	pack.EncodeFrame(make([]byte, 1024), pack.DefaultZstdLevel)
	os.Exit(packagetest.RunWithLockRegistry(m))
}
