package loadfile

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadManifestJSONLReturnsExactRetainedAuthority(t *testing.T) {
	manifest := Manifest{
		ProfileSHA256: strings.Repeat("a", 64), MappingSHA256: strings.Repeat("b", 64),
		Mapping: Mapping{Contract: MappingContractV1},
		Volumes: []Volume{{Name: "VOL001", DeclaredRoot: "VOL001", Ordinal: 1}},
		Records: []Record{{RowID: "row-a", DocID: "DOC-A", LoadFile: "VOL001.dat", RowOrdinal: 1}},
		Images:  []ImageRef{{ImageKey: "DOC-A", Volume: "VOL001", RelPath: "IMAGES/A.tif", PageOrdinal: 1}},
		Files:   []FileRef{{Role: "raw_load_file", Volume: "VOL001", RelPath: "VOL001.dat", Status: "available"}},
	}
	var encoded bytes.Buffer
	require.NoError(t, manifest.WriteJSONL(&encoded))
	digest, err := manifest.SHA256()
	require.NoError(t, err)
	read, err := ReadManifestJSONL(bytes.NewReader(encoded.Bytes()), digest)
	require.NoError(t, err)
	var replay bytes.Buffer
	require.NoError(t, read.WriteJSONL(&replay))
	require.Equal(t, encoded.Bytes(), replay.Bytes())
	require.Equal(t, "DOC-A", read.Records[0].DocID)

	changed := append([]byte(nil), encoded.Bytes()...)
	changed = bytes.Replace(changed, []byte("DOC-A"), []byte("DOC-B"), 1)
	_, err = ReadManifestJSONL(bytes.NewReader(changed), digest)
	require.ErrorIs(t, err, ErrMalformedInput)
	_, err = ReadManifestJSONL(bytes.NewReader(bytes.TrimSuffix(encoded.Bytes(), []byte{'\n'})), digest)
	require.ErrorIs(t, err, ErrMalformedInput)
	_, err = ReadManifestJSONL(bytes.NewReader(append(encoded.Bytes(), []byte(`{"kind":"unknown"}`)...)), digest)
	require.ErrorIs(t, err, ErrMalformedInput)
	lines := bytes.Split(bytes.TrimSuffix(encoded.Bytes(), []byte{'\n'}), []byte{'\n'})
	lines[1], lines[2] = lines[2], lines[1]
	outOfOrder := append(bytes.Join(lines, []byte{'\n'}), '\n')
	outOfOrderDigest := sha256.Sum256(outOfOrder)
	_, err = ReadManifestJSONL(bytes.NewReader(outOfOrder), hex.EncodeToString(outOfOrderDigest[:]))
	require.ErrorIs(t, err, ErrMalformedInput)
	_, err = ReadManifestJSONL(bytes.NewReader(bytes.Repeat([]byte{'x'}, maxRetainedManifestLine+2)), digest)
	require.ErrorIs(t, err, ErrLoadfileLimit)
}
