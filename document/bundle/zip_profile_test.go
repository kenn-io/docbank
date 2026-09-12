package bundle

import (
	"archive/zip"
	"bytes"
	"fmt"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestZIP64DirectoryCountAndUnsafeEntryRefusal(t *testing.T) {
	var dst bytes.Buffer
	writer := zip.NewWriter(&dst)
	for i := range 65535 {
		h := &zip.FileHeader{Name: fmt.Sprintf("documents/%06d", i), Method: zip.Store, ModifiedDate: 33} //nolint:staticcheck // Exact DOS date without timestamp extras is the bundle profile.
		h.SetMode(0600)
		_, err := writer.CreateHeader(h)
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	raw := dst.Bytes()
	entries, err := readDirectory(bytes.NewReader(raw), int64(len(raw)))
	require.NoError(t, err)
	require.Len(t, entries, 65535)
	changed := bytes.Clone(raw)
	le.PutUint64(changed[len(changed)-98+32:], 65536)
	_, err = readDirectory(bytes.NewReader(changed), int64(len(changed)))
	require.Error(t, err)
	for _, name := range []string{"../escape", "/absolute", "documents/con", "documents/a.", "documents/a\\b", "documents//x"} {
		t.Run(name, func(t *testing.T) {
			var dst bytes.Buffer
			z := zip.NewWriter(&dst)
			for _, entry := range []string{name, "metadata.csv", "bundle.json"} {
				h := &zip.FileHeader{Name: entry, Method: zip.Store, ModifiedDate: 33} //nolint:staticcheck // Exact DOS date without timestamp extras is the bundle profile.
				h.SetMode(0600)
				_, err := z.CreateHeader(h)
				require.NoError(t, err)
			}
			require.NoError(t, z.Close())
			_, err := readDirectory(bytes.NewReader(dst.Bytes()), int64(dst.Len()))
			require.Error(t, err)
		})
	}
}

// sparseZIPFixture represents zero-filled gaps without relying on filesystem
// sparse-file support (in particular, Windows must not allocate GiB here).
type sparseZIPFixture map[int64][]byte

func (s sparseZIPFixture) WriteAt(b []byte, offset int64) (int, error) {
	s[offset] = bytes.Clone(b)
	return len(b), nil
}

func (s sparseZIPFixture) ReadAt(b []byte, offset int64) (int, error) {
	clear(b)
	for start, value := range s {
		lower, upper := max(offset, start), min(offset+int64(len(b)), start+int64(len(value)))
		if lower < upper {
			copy(b[lower-offset:upper-offset], value[lower-start:upper-start])
		}
	}
	return len(b), nil
}

// Virtual sparse archives exercise the actual 32-bit offset and descriptor
// boundary while retaining only their bounded ZIP records.
func TestZIP64LargeEntryAndOffsetBoundaries(t *testing.T) {
	for _, size := range []uint64{0xffffffff, 0x100000000} {
		t.Run(strconv.FormatUint(size, 10), func(t *testing.T) {
			file := sparseZIPFixture{}
			var err error
			var directory bytes.Buffer
			var offset uint64
			for i, name := range []string{"role", "metadata.csv", "bundle.json"} {
				payload := uint64(0)
				if i == 0 {
					payload = size
				}
				local := make([]byte, 30+len(name))
				le.PutUint32(local, 0x04034b50)
				le.PutUint16(local[4:], 20)
				le.PutUint16(local[6:], 8)
				le.PutUint16(local[12:], 33)
				le.PutUint16(local[26:], uint16(len(name)))
				copy(local[30:], name)
				_, err = file.WriteAt(local, int64(offset))
				require.NoError(t, err)
				descriptor := make([]byte, 16)
				if payload > 0xffffffff {
					descriptor = make([]byte, 24)
				}
				le.PutUint32(descriptor, 0x08074b50)
				if len(descriptor) == 16 {
					le.PutUint32(descriptor[8:], uint32(payload))
					le.PutUint32(descriptor[12:], uint32(payload))
				} else {
					le.PutUint64(descriptor[8:], payload)
					le.PutUint64(descriptor[16:], payload)
				}
				_, err = file.WriteAt(descriptor, int64(offset)+int64(len(local))+int64(payload))
				require.NoError(t, err)
				var extra bytes.Buffer
				append64 := func(v uint64) { b := make([]byte, 8); le.PutUint64(b, v); _, _ = extra.Write(b) }
				if payload >= 0xffffffff {
					append64(payload)
					append64(payload)
				}
				if offset >= 0xffffffff {
					append64(offset)
				}
				rawExtra := []byte{}
				if extra.Len() > 0 {
					rawExtra = make([]byte, 4+extra.Len())
					le.PutUint16(rawExtra, 1)
					le.PutUint16(rawExtra[2:], uint16(extra.Len()))
					copy(rawExtra[4:], extra.Bytes())
				}
				central := make([]byte, 46)
				le.PutUint32(central, 0x02014b50)
				le.PutUint16(central[4:], 3<<8|20)
				version := uint16(20)
				if len(rawExtra) > 0 {
					version = 45
				}
				le.PutUint16(central[6:], version)
				le.PutUint16(central[8:], 8)
				le.PutUint16(central[14:], 33)
				le.PutUint32(central[20:], uint32(min(payload, 0xffffffff)))
				le.PutUint32(central[24:], uint32(min(payload, 0xffffffff)))
				le.PutUint16(central[28:], uint16(len(name)))
				le.PutUint16(central[30:], uint16(len(rawExtra)))
				le.PutUint32(central[38:], 0100600<<16)
				le.PutUint32(central[42:], uint32(min(offset, 0xffffffff)))
				_, _ = directory.Write(central)
				_, _ = directory.WriteString(name)
				_, _ = directory.Write(rawExtra)
				offset += uint64(len(local)) + payload + uint64(len(descriptor))
			}
			_, err = file.WriteAt(directory.Bytes(), int64(offset))
			require.NoError(t, err)
			trailer := make([]byte, 98)
			le.PutUint32(trailer, 0x06064b50)
			le.PutUint64(trailer[4:], 44)
			le.PutUint16(trailer[12:], 45)
			le.PutUint16(trailer[14:], 45)
			le.PutUint64(trailer[24:], 3)
			le.PutUint64(trailer[32:], 3)
			le.PutUint64(trailer[40:], uint64(directory.Len()))
			le.PutUint64(trailer[48:], offset)
			le.PutUint32(trailer[56:], 0x07064b50)
			le.PutUint64(trailer[64:], offset+uint64(directory.Len()))
			le.PutUint32(trailer[72:], 1)
			le.PutUint32(trailer[76:], 0x06054b50)
			le.PutUint16(trailer[84:], 3)
			le.PutUint16(trailer[86:], 3)
			le.PutUint32(trailer[88:], uint32(directory.Len()))
			le.PutUint32(trailer[92:], 0xffffffff)
			_, err = file.WriteAt(trailer, int64(offset)+int64(directory.Len()))
			require.NoError(t, err)
			entries, err := readDirectory(file, int64(offset)+int64(directory.Len())+98)
			require.NoError(t, err)
			require.Len(t, entries, 3)
			require.Equal(t, int64(size), entries[0].size)
		})
	}
}

func TestArchiveRejectsCompressionDuplicateAndSymlinkEntries(t *testing.T) {
	for _, kind := range []string{"compressed", "duplicate", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			var dst bytes.Buffer
			z := zip.NewWriter(&dst)
			for _, name := range []string{"role", "metadata.csv", "bundle.json"} {
				h := &zip.FileHeader{Name: name, Method: zip.Store, ModifiedDate: 33} //nolint:staticcheck // Exact DOS date without timestamp extras is the bundle profile.
				h.SetMode(0600)
				switch kind {
				case "compressed":
					h.Method = zip.Deflate
				case "duplicate":
					h.Name = "role"
				case "symlink":
					h.SetMode(0600 | 1<<27)
				}
				w, err := z.CreateHeader(h)
				require.NoError(t, err)
				_, err = w.Write([]byte("synthetic"))
				require.NoError(t, err)
			}
			require.NoError(t, z.Close())
			_, err := readDirectory(bytes.NewReader(dst.Bytes()), int64(dst.Len()))
			require.Error(t, err)
		})
	}
}
