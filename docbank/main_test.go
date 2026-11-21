package docbank_test

import (
	"bytes"
	"os"
	"testing"

	"go.kenn.io/kit/pack"

	"go.kenn.io/docbank/internal/packagetest"
	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/sqlitetest"
)

func TestMain(m *testing.M) {
	sqlitetest.DisableSync.Store(true)
	// Kit shares one zstd encoder per level. Create it here so its channels
	// do not belong to whichever synctest bubble compresses a frame first.
	pack.EncodeFrame(bytes.Repeat([]byte("synctest warm-up "), 512), pack.DefaultZstdLevel)
	os.Exit(pgtest.Finish(packagetest.RunShardsWithLockRegistry(m)))
}
