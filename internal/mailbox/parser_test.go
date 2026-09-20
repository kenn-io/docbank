package mailbox

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"github.com/stretchr/testify/require"
	"io"
	"os"
	"strings"
	"testing"
)

const separator = "From sender@example.test Sat Sep 12 10:00:00 2026\n"

func TestMailboxDialectOccurrencesAndOversizeRecovery(t *testing.T) {
	source := separator + "Subject: duplicate\nMessage-ID: <same@example.test>\n\n>From first\n>>From second\n" + separator + "Subject: duplicate\nMessage-ID: <same@example.test>\n\n>From first\n>>From second\n"
	for _, dialect := range []string{"mboxrd", "mboxo"} {
		t.Run(dialect, func(t *testing.T) {
			spool := t.TempDir()
			r, err := NewScanner(strings.NewReader(source), dialect, spool, 128<<20)
			require.NoError(t, err)
			var hashes []string
			for i := range 2 {
				m, err := r.Next(t.Context())
				require.NoError(t, err)
				require.Equal(t, int64(i+1), m.Sequence)
				require.Equal(t, int64(i*len(source)/2), m.Start)
				require.Equal(t, hashBytes([]byte(source[i*len(source)/2:(i+1)*len(source)/2])), m.RawSHA256)
				content, err := io.ReadAll(m.EML)
				require.NoError(t, err)
				require.Contains(t, string(content), "\nFrom first\n")
				if dialect == "mboxrd" {
					require.Contains(t, string(content), "\n>From second\n")
				} else {
					require.Contains(t, string(content), "\n>>From second\n")
				}
				require.Equal(t, hashBytes(content), m.EMLSHA256)
				hashes = append(hashes, m.EMLSHA256)
				require.NoError(t, m.Close())
			}
			require.Equal(t, hashes[0], hashes[1])
			_, err = r.Next(t.Context())
			require.ErrorIs(t, err, io.EOF)
			files, err := os.ReadDir(spool)
			require.NoError(t, err)
			require.Empty(t, files)
		})
	}
	spool := t.TempDir()
	r, err := NewScanner(strings.NewReader(separator+"Subject: huge\n\n"+strings.Repeat("x", 200000)+"\n"+separator+"Subject: good\n\nhello\n"), "mboxrd", spool, 100)
	require.NoError(t, err)
	first, err := r.Next(t.Context())
	require.NoError(t, err)
	require.Equal(t, "message_too_large", first.Rejection)
	require.Nil(t, first.EML)
	require.Equal(t, int64(len("Subject: huge\n\n")+200001), first.EMLSize)
	require.Equal(t, hashBytes([]byte("Subject: huge\n\n"+strings.Repeat("x", 200000)+"\n")), first.EMLSHA256)
	next, err := r.Next(t.Context())
	require.NoError(t, err)
	require.Empty(t, next.Rejection)
	require.NoError(t, next.Close())
	invalid, err := NewScanner(strings.NewReader("not a mailbox\n"), "mboxrd", spool, 100)
	require.NoError(t, err)
	_, err = invalid.Next(t.Context())
	require.Error(t, err)
}

func TestTakeoutPreflightCRCAndForgedDirectoryCounts(t *testing.T) {
	original := syntheticZIP(t, "Takeout/Mail/All.mbox", 0600, separator+"Subject: Synthetic\n\nBody\n")
	central := bytes.Index(original, []byte{'P', 'K', 1, 2})
	require.Positive(t, central)
	badCRC := bytes.Clone(original)
	binary.LittleEndian.PutUint32(badCRC[central+16:], 0)
	_, err := PreflightZIP(t.Context(), bytes.NewReader(badCRC), int64(len(badCRC)), DefaultArchiveLimits())
	require.ErrorIs(t, err, zip.ErrChecksum)
	for _, count := range []uint16{0, 2, 1001} {
		bad := bytes.Clone(original)
		binary.LittleEndian.PutUint16(bad[len(bad)-12:], count)
		_, err = PreflightZIP(t.Context(), bytes.NewReader(bad), int64(len(bad)), DefaultArchiveLimits())
		require.Error(t, err)
	}
	// A valid small ZIP64 archive exercises the same path as large offsets,
	// without claiming this fixture measures multi-gigabyte throughput.
	end := original[len(original)-22:]
	z64 := bytes.Clone(original[:len(original)-22])
	position := len(z64)
	record := make([]byte, 56)
	binary.LittleEndian.PutUint32(record, 0x06064b50)
	binary.LittleEndian.PutUint64(record[4:], 44)
	binary.LittleEndian.PutUint64(record[24:], 1)
	binary.LittleEndian.PutUint64(record[32:], 1)
	binary.LittleEndian.PutUint64(record[40:], uint64(binary.LittleEndian.Uint32(end[12:])))
	binary.LittleEndian.PutUint64(record[48:], uint64(binary.LittleEndian.Uint32(end[16:])))
	z64 = append(z64, record...)
	locator := make([]byte, 20)
	binary.LittleEndian.PutUint32(locator, 0x07064b50)
	binary.LittleEndian.PutUint64(locator[8:], uint64(position))
	binary.LittleEndian.PutUint32(locator[16:], 1)
	z64 = append(z64, locator...)
	end = bytes.Clone(end)
	binary.LittleEndian.PutUint16(end[8:], 65535)
	binary.LittleEndian.PutUint16(end[10:], 65535)
	binary.LittleEndian.PutUint32(end[12:], 0xffffffff)
	binary.LittleEndian.PutUint32(end[16:], 0xffffffff)
	z64 = append(z64, end...)
	entries, err := PreflightZIP(t.Context(), bytes.NewReader(z64), int64(len(z64)), DefaultArchiveLimits())
	require.NoError(t, err)
	require.Len(t, entries, 1)
}
func syntheticZIP(t *testing.T, name string, mode os.FileMode, content string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	h := &zip.FileHeader{Name: name, Method: zip.Deflate}
	h.SetMode(mode)
	entry, err := w.CreateHeader(h)
	require.NoError(t, err)
	_, err = io.WriteString(entry, content)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return b.Bytes()
}
func TestTakeoutPreflightRejectsUnsafeAndActualExpansion(t *testing.T) {
	for _, name := range []string{"../escape.mbox", "/absolute.mbox", "C:/drive.mbox", "a/../../escape.mbox"} {
		b := syntheticZIP(t, name, 0600, separator+"Subject: A\n\nBody\n")
		_, err := PreflightZIP(t.Context(), bytes.NewReader(b), int64(len(b)), DefaultArchiveLimits())
		require.Error(t, err)
	}
	b := syntheticZIP(t, "Takeout/Mail/All mail.mbox", os.ModeSymlink|0600, "target")
	_, err := PreflightZIP(t.Context(), bytes.NewReader(b), int64(len(b)), DefaultArchiveLimits())
	require.Error(t, err)
	b = syntheticZIP(t, "Takeout/Mail/All mail.mbox", 0600, separator+"Subject: A\n\nBody\n")
	entries, err := PreflightZIP(t.Context(), bytes.NewReader(b), int64(len(b)), DefaultArchiveLimits())
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, hashBytes([]byte(separator+"Subject: A\n\nBody\n")), entries[0].SHA256)
	limits := DefaultArchiveLimits()
	limits.ExpandedBytes = 10
	_, err = PreflightZIP(t.Context(), bytes.NewReader(b), int64(len(b)), limits)
	require.Error(t, err)
	b = syntheticZIP(t, "readme.txt", 0600, "no messages")
	_, err = PreflightZIP(t.Context(), bytes.NewReader(b), int64(len(b)), DefaultArchiveLimits())
	require.Error(t, err)
}
