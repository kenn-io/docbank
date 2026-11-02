package processing_test

import (
	"bytes"
	"os"
	"testing"

	"go.kenn.io/kit/pack"
)

func TestMain(m *testing.M) {
	// Kit shares one zstd encoder per level. Create it here so its channels
	// do not belong to whichever synctest bubble compresses a frame first.
	pack.EncodeFrame(bytes.Repeat([]byte("synctest warm-up "), 512), pack.DefaultZstdLevel)
	os.Exit(m.Run())
}
