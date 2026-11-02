package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/atomicfile"
)

func TestRuntimeStoreWriteCheckStaysOutsideDiscoveryNamespaces(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	store := RuntimeStore{Dir: t.TempDir(), Prefix: "tool"}
	prefix, err := store.validatePrefix()
	require.NoError(err)
	probe, err := os.CreateTemp(store.Dir, fmt.Sprintf(runtimeWriteCheckPattern, prefix))
	require.NoError(err)
	t.Cleanup(func() {
		_ = probe.Close()
		_ = os.Remove(probe.Name())
	})

	records, err := store.List()
	require.NoError(err)
	assert.Empty(records)
	name := filepath.Base(probe.Name())
	_, isRecord := pidFromName(prefix, name)
	assert.False(isRecord)
	assert.NotEqual(prefix+".lock", name)
	assert.NotEqual(prefix+".listen.lock", name)
}

func TestRuntimeStoreWriteReturnsPathWhenRecordWasPublishedButNotDurable(t *testing.T) {
	require := require.New(t)
	store := RuntimeStore{Dir: t.TempDir()}
	original := writeAtomicFile
	writeAtomicFile = func(path string, data []byte, opts ...atomicfile.Option) error {
		if err := original(path, data, opts...); err != nil {
			return err
		}
		return errors.Join(atomicfile.ErrNotDurable, errors.New("injected directory sync failure"))
	}
	t.Cleanup(func() { writeAtomicFile = original })

	path, err := store.Write(RuntimeRecord{PID: 1, Address: "127.0.0.1:7474"})

	require.ErrorIs(err, atomicfile.ErrNotDurable)
	assert.Equal(t, filepath.Join(store.Dir, "daemon.1.json"), path)
	_, statErr := os.Stat(path)
	require.NoError(statErr)
}
