//go:build darwin || linux || windows

package atomicfile_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/safefileio"
)

func TestWithPrivateProducesValidatedPrivateFile(t *testing.T) {
	for name, write := range map[string]func(string, []byte, ...atomicfile.Option) error{
		"WriteFile": atomicfile.WriteFile,
		"WriteNew":  atomicfile.WriteNew,
	} {
		t.Run(name, func(t *testing.T) {
			require := require.New(t)
			dir := t.TempDir()
			target := filepath.Join(dir, "secret")

			require.NoError(write(target, []byte("token"), atomicfile.WithPrivate()))

			file, err := safefileio.OpenCurrentUserFile(target)
			require.NoError(err)
			defer func() { _ = file.Close() }()
			require.NoError(safefileio.ValidatePrivateCurrentUserFile(file))
			assert.Equal(t, "token", readString(t, target))
			assert.Equal(t, []string{"secret"}, entryNames(t, dir))
		})
	}
}

func TestConflictingOptionsWriteNothing(t *testing.T) {
	tests := []struct {
		name string
		opts []atomicfile.Option
	}{
		{name: "WithPrivate and WithPerm", opts: []atomicfile.Option{atomicfile.WithPrivate(), atomicfile.WithPerm(0o600)}},
		{name: "WithPrivate and WithCreatePerm", opts: []atomicfile.Option{atomicfile.WithPrivate(), atomicfile.WithCreatePerm(0o600)}},
		{name: "WithPrivate and WithPreserveMode", opts: []atomicfile.Option{atomicfile.WithPrivate(), atomicfile.WithPreserveMode()}},
		{name: "WithPerm and WithCreatePerm", opts: []atomicfile.Option{atomicfile.WithPerm(0o600), atomicfile.WithCreatePerm(0o644)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := filepath.Join(t.TempDir(), "target")

			require.Error(t, atomicfile.WriteFile(target, []byte("data"), tt.opts...))
			require.Error(t, atomicfile.WriteNew(target, []byte("data"), tt.opts...))
			assert.NoFileExists(t, target)
		})
	}
}
