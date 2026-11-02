package spill

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadFileRejectsLengthPastMaxInt(t *testing.T) {
	if strconv.IntSize == 64 {
		t.Skip("a length past math.MaxInt needs a 32-bit int")
	}
	require := require.New(t)
	file, err := os.Create(filepath.Join(t.TempDir(), "records"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(file.Close()) })

	// One record whose length is 2^31. Truncate extends the file sparsely, so
	// the record bytes take no disk space and the length passes the payload
	// check.
	length := uint64(math.MaxInt) + 1
	header := binary.AppendUvarint(nil, length)
	_, err = file.Write(header)
	require.NoError(err)
	size := int64(len(header)) + int64(length)
	require.NoError(file.Truncate(size))

	err = readFile(file, size, 1, int64(length), func(string, error) bool { return true })
	require.ErrorContains(err, "length 2147483648 does not fit in an int")
}

func TestAppendRejectsRecordPastMaxIntCount(t *testing.T) {
	require := require.New(t)
	l := New(t.TempDir(), "spill-*", 0)
	t.Cleanup(func() { require.NoError(l.Close()) })
	// 2^31 appends are too slow for a test on 32-bit platforms, so start the
	// count at the boundary.
	l.count = math.MaxInt

	require.ErrorContains(l.Append("a"), "already holds")
	require.Equal(math.MaxInt, l.Len())
	require.False(l.Spilled())
}
