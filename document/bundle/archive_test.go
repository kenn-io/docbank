package bundle

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Corruption, an unexpected entry, or a ZIP prefix must never earn a receipt.
func TestArchiveDeterministicReadbackAndTamperRefusal(t *testing.T) {
	body := []byte("synthetic original\n")
	h := sha256.Sum256(body)
	d := Document{NodeID: 2, VersionID: "c0a7ac5a-a410-44ee-9ad1-17200487e37b", SHA256: hex.EncodeToString(h[:]), Size: int64(len(body)), Name: "=formula.txt", Path: "/synthetic/=formula.txt", MediaType: "text/plain"}
	d.Roles = []Role{{Role: "original", Status: "available", Path: "documents/2/" + d.VersionID + "/original", SHA256: d.SHA256, Size: d.Size}}
	plan := Plan{Format: Format, ID: "0d845f53-bacc-48ae-ac2e-0cefd5e2958c", VaultID: "620e0094-c014-45d3-8974-851476415a29", Toolchain: "synthetic", Total: 1, RoleEntries: 1, RoleBytes: d.Size, Roles: []RolePolicy{{Role: "original"}}}
	walk := func(visit func(Document) error) error { return visit(d) }
	var err error
	plan.Fingerprint, err = Fingerprint(plan, walk)
	require.NoError(t, err)
	open := func(Role) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	first := filepath.Join(t.TempDir(), "bundle.zip")
	f, err := os.Create(first)
	require.NoError(t, err)
	receipt, err := Write(t.Context(), f, plan, walk, open, nil)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	f, err = os.Open(first)
	require.NoError(t, err)
	verified, err := Verify(t.Context(), f, receipt.Size, plan.Fingerprint)
	require.NoError(t, err)
	require.Equal(t, receipt, verified)
	require.NoError(t, f.Close())
	raw, err := os.ReadFile(first)
	require.NoError(t, err)
	z, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	require.NoError(t, err)
	require.Len(t, z.File, 4)
	for _, file := range z.File {
		if file.Name == "metadata.csv" {
			r, e := file.Open()
			require.NoError(t, e)
			csv, e := io.ReadAll(r)
			require.NoError(t, e)
			require.NoError(t, r.Close())
			require.Contains(t, string(csv), "'=formula.txt")
		}
	}
	for _, mutated := range [][]byte{append([]byte("prefix"), raw...), append(bytes.Clone(raw), 0), raw[:len(raw)-1]} {
		_, err = Verify(t.Context(), bytes.NewReader(mutated), int64(len(mutated)), plan.Fingerprint)
		require.Error(t, err)
	}
	changed := bytes.Clone(raw)
	offset := bytes.Index(changed, body)
	require.Positive(t, offset)
	changed[offset] ^= 1
	_, err = Verify(t.Context(), bytes.NewReader(changed), int64(len(changed)), plan.Fingerprint)
	require.Error(t, err)
	second, err := os.Create(filepath.Join(t.TempDir(), "second.zip"))
	require.NoError(t, err)
	again, err := Write(t.Context(), second, plan, walk, open, nil)
	require.NoError(t, err)
	require.NoError(t, second.Close())
	require.Equal(t, receipt, again)
}
