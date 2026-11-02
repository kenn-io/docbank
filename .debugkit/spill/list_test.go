package spill_test

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/spill"
)

func collect(l *spill.List) ([]string, error) {
	var records []string
	for record, err := range l.All() {
		if err != nil {
			return records, err
		}
		records = append(records, record)
	}
	return records, nil
}

// onlyFile returns the path of the single entry in dir.
func onlyFile(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	return filepath.Join(dir, entries[0].Name())
}

func TestListUnderLimitStaysInMemory(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	// 10 payload bytes plus 16 bytes for each of the 4 records.
	l := spill.New(dir, "spill-*", 74)
	t.Cleanup(func() { require.NoError(l.Close()) })

	// The records reach the limit exactly, which still fits in memory.
	for _, s := range []string{"abc", "", "defg", "hij"} {
		require.NoError(l.Append(s))
	}

	records, err := collect(l)
	require.NoError(err)
	assert.Equal(t, []string{"abc", "", "defg", "hij"}, records)
	assert.Equal(t, 4, l.Len())
	assert.Equal(t, int64(10), l.Size())
	assert.False(t, l.Spilled())
	entries, err := os.ReadDir(dir)
	require.NoError(err)
	assert.Empty(t, entries)
}

func TestListOverLimitMovesToPrivateFile(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	// Two records of 2 bytes fill the limit with their 16-byte overhead.
	l := spill.New(dir, "spill-*.tmp", 36)
	t.Cleanup(func() { require.NoError(l.Close()) })

	require.NoError(l.Append("ab"))
	require.NoError(l.Append("cd"))
	require.False(l.Spilled())
	require.NoError(l.Append("e"))
	require.True(l.Spilled())

	path := onlyFile(t, dir)
	assert.True(t, strings.HasPrefix(filepath.Base(path), "spill-"))
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		require.NoError(err)
		assert.Equal(t, fs.FileMode(0o600), info.Mode().Perm())
	}

	require.NoError(l.Append("fgh"))
	records, err := collect(l)
	require.NoError(err)
	assert.Equal(t, []string{"ab", "cd", "e", "fgh"}, records)

	// Appending after a read continues the same list.
	require.NoError(l.Append("ij"))
	records, err = collect(l)
	require.NoError(err)
	assert.Equal(t, []string{"ab", "cd", "e", "fgh", "ij"}, records)
	assert.Equal(t, 5, l.Len())
	assert.Equal(t, int64(10), l.Size())
}

func TestListIterationYieldsRecordsAppendedBeforeItStarted(t *testing.T) {
	require := require.New(t)
	l := spill.New(t.TempDir(), "spill-*", 34)
	t.Cleanup(func() { require.NoError(l.Close()) })
	require.NoError(l.Append("a"))
	require.NoError(l.Append("b"))

	// The first pass starts in memory and moves the list to disk midway; the
	// second starts on disk and appends to the file.
	for range 2 {
		var seen []string
		for record, err := range l.All() {
			require.NoError(err)
			seen = append(seen, record)
			require.NoError(l.Append(record + record))
		}
		assert.Len(t, seen, l.Len()/2)
	}
	assert.True(t, l.Spilled())

	records, err := collect(l)
	require.NoError(err)
	assert.Equal(t, []string{"a", "b", "aa", "bb", "aa", "bb", "aaaa", "bbbb"}, records)
}

func TestListCountsEmptyRecordsTowardLimit(t *testing.T) {
	require := require.New(t)
	l := spill.New(t.TempDir(), "spill-*", 32)
	t.Cleanup(func() { require.NoError(l.Close()) })

	require.NoError(l.Append(""))
	require.NoError(l.Append(""))
	require.False(l.Spilled())
	require.NoError(l.Append(""))
	require.True(l.Spilled())
	assert.Equal(t, int64(0), l.Size())

	records, err := collect(l)
	require.NoError(err)
	assert.Equal(t, []string{"", "", ""}, records)
}

func TestListRoundTripsArbitraryBytes(t *testing.T) {
	var everyByte strings.Builder
	for b := range 256 {
		everyByte.WriteByte(byte(b))
	}
	values := []string{"", "a\x00b", "\x00", "line\n", "\r\n", "", everyByte.String(), strings.Repeat("x", 200_000), ""}

	require := require.New(t)
	l := spill.New(t.TempDir(), "spill-*", -1)
	t.Cleanup(func() { require.NoError(l.Close()) })
	for _, v := range values {
		require.NoError(l.Append(v))
	}
	require.True(l.Spilled())

	records, err := collect(l)
	require.NoError(err)
	assert.Equal(t, values, records)
}

func TestListCloseRemovesFile(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	l := spill.New(dir, "spill-*", 0)
	require.NoError(l.Append("record"))
	onlyFile(t, dir)

	require.NoError(l.Close())
	entries, err := os.ReadDir(dir)
	require.NoError(err)
	assert.Empty(t, entries)

	require.NoError(l.Close())
	require.ErrorIs(l.Append("more"), fs.ErrClosed)
	_, err = collect(l)
	require.ErrorIs(err, fs.ErrClosed)
}

func TestListAppendReportsFileCreationFailure(t *testing.T) {
	require := require.New(t)
	l := spill.New(filepath.Join(t.TempDir(), "missing"), "spill-*", 17)
	t.Cleanup(func() { require.NoError(l.Close()) })
	require.NoError(l.Append("a"))

	require.ErrorIs(l.Append("b"), fs.ErrNotExist)

	// The failed append leaves the list as it was.
	assert.False(t, l.Spilled())
	assert.Equal(t, 1, l.Len())
	records, err := collect(l)
	require.NoError(err)
	assert.Equal(t, []string{"a"}, records)
}

func TestListReadReportsDamagedFile(t *testing.T) {
	short := []string{"ab", "cd", "ef"}
	shortFile := "\x02ab\x02cd\x02ef"
	// A 200-byte record takes a two-byte length, c8 01. Rewriting it as the
	// one-byte length 0x64 ('d', 100) splits the record into two records of
	// 100 bytes with the right total payload. The records then end one byte
	// before the data does, whether or not that last byte is still there.
	long := []string{strings.Repeat("d", 200), ""}
	longFile := "\xc8\x01" + strings.Repeat("d", 200) + "\x00"

	tests := map[string]struct {
		records []string
		file    string
		damage  func([]byte) []byte
		wantErr error
		wantMsg string
	}{
		"truncated inside a record": {
			records: short,
			file:    shortFile,
			damage:  func(b []byte) []byte { return b[:len(b)-1] },
			wantErr: io.ErrUnexpectedEOF,
		},
		"truncated before a length": {
			records: short,
			file:    shortFile,
			damage:  func(b []byte) []byte { return b[:6] },
			wantErr: io.ErrUnexpectedEOF,
		},
		"length past the payload": {
			records: short,
			file:    shortFile,
			damage:  func(b []byte) []byte { b[3] = 0x05; return b },
			wantMsg: "exceeds the 4 payload bytes left",
		},
		"lengths short of the payload": {
			records: short,
			file:    shortFile,
			damage:  func(b []byte) []byte { b[6] = 0x01; return b },
			wantMsg: "1 fewer payload bytes",
		},
		"records end before the data": {
			records: long,
			file:    longFile,
			damage:  func(b []byte) []byte { b[0] = 0x64; return b },
			wantMsg: "records end at byte 202 of the 203 bytes written",
		},
		"records end at a truncated end": {
			records: long,
			file:    longFile,
			damage:  func(b []byte) []byte { b[0] = 0x64; return b[:len(b)-1] },
			wantMsg: "records end at byte 202 of the 203 bytes written",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			require := require.New(t)
			dir := t.TempDir()
			l := spill.New(dir, "spill-*", 0)
			t.Cleanup(func() { require.NoError(l.Close()) })
			for _, s := range tt.records {
				require.NoError(l.Append(s))
			}
			// Reading flushes the records to the file.
			_, err := collect(l)
			require.NoError(err)

			file, err := openShared(onlyFile(t, dir))
			require.NoError(err)
			data, err := io.ReadAll(file)
			require.NoError(err)
			require.Equal([]byte(tt.file), data)
			damaged := tt.damage(data)
			require.NoError(file.Truncate(0))
			_, err = file.WriteAt(damaged, 0)
			require.NoError(err)
			require.NoError(file.Close())

			_, err = collect(l)
			if tt.wantErr != nil {
				require.ErrorIs(err, tt.wantErr)
			}
			require.ErrorContains(err, tt.wantMsg)
		})
	}
}
