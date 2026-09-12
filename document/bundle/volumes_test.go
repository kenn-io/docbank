package bundle

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestArchiveBoundedVolumesReconcileEveryRole(t *testing.T) {
	body := []byte("Synthetic volume payload\n")
	sum := sha256.Sum256(body)
	d := Document{NodeID: 2, VersionID: "c0a7ac5a-a410-44ee-9ad1-17200487e37b", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(body)), Name: "source.txt", Path: "/source.txt", MediaType: "text/plain"}
	d.Roles = []Role{{Role: "original", Status: "available", Path: "documents/2/" + d.VersionID + "/original", SHA256: d.SHA256, Size: d.Size}, {Role: "text", Status: "available", Path: "documents/2/" + d.VersionID + "/text.md", SHA256: d.SHA256, Size: d.Size}}
	// Use wire input so absence of the new contract fails on output behavior.
	for i := range d.Roles {
		d.Roles[i].Volume = i + 1
	}
	plan := Plan{Format: Format, ID: "0d845f53-bacc-48ae-ac2e-0cefd5e2958c", VaultID: "620e0094-c014-45d3-8974-851476415a29", Toolchain: "synthetic", Total: 1, RoleEntries: 2, RoleBytes: d.Size * 2, Roles: []RolePolicy{{Role: "original"}, {Role: "text"}}}
	raw, err := json.Marshal(plan)
	require.NoError(t, err)
	raw = append(raw[:len(raw)-1], []byte(",\"volume_limits\":{\"role_bytes\":1024,\"roles\":1},\"volumes\":2}")...)
	require.NoError(t, json.Unmarshal(raw, &plan))
	walk := func(visit func(Document) error) error { return visit(d) }
	plan.Fingerprint, err = Fingerprint(plan, walk)
	require.NoError(t, err)
	file, err := os.Create(filepath.Join(t.TempDir(), "volumes.zip"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, file.Close()) })
	receipt, err := Write(t.Context(), file, plan, walk, func(Role) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }, nil)
	require.NoError(t, err)
	outer, err := zip.NewReader(file, receipt.Size)
	require.NoError(t, err)
	require.Equal(t, "volumes/000001.zip", outer.File[0].Name)
	require.Equal(t, "volumes/000002.zip", outer.File[1].Name)
	var payloads [][]byte
	for i, entry := range outer.File[:2] {
		stream, e := entry.Open()
		require.NoError(t, e)
		payload, e := io.ReadAll(stream)
		require.NoError(t, e)
		require.NoError(t, stream.Close())
		payloads = append(payloads, payload)
		manifest, e := VerifyVolume(t.Context(), bytes.NewReader(payload), int64(len(payload)), plan.Fingerprint)
		require.NoError(t, e)
		require.Equal(t, i, manifest.FirstRole)
		require.Equal(t, 1, manifest.Roles)
		volume, e := zip.NewReader(bytes.NewReader(payload), int64(len(payload)))
		require.NoError(t, e)
		require.Len(t, volume.File, 3)
		require.Equal(t, d.Roles[i].Path, volume.File[0].Name)
		require.Equal(t, "volume.json", volume.File[1].Name)
		require.Equal(t, "SHA256SUMS", volume.File[2].Name)
	}
	_, err = Verify(t.Context(), file, receipt.Size, plan.Fingerprint)
	require.NoError(t, err)
	// Recompute the complete outer ZIP and checksums after swapping two valid
	// volumes. Only their source-linked index/range reconciliation exposes this.
	swapped := repackVolumeArchive(t, outer, map[string][]byte{"volumes/000001.zip": payloads[1], "volumes/000002.zip": payloads[0]})
	_, err = Verify(t.Context(), bytes.NewReader(swapped), int64(len(swapped)), plan.Fingerprint)
	require.ErrorIs(t, err, ErrInvalidArchive)
	plan.VolumeLimits.RoleBytes = int64(len(body)) - 1
	validator := RowValidator{Plan: plan}
	require.ErrorIs(t, validator.Add(d), ErrLimit)
}

func repackVolumeArchive(t *testing.T, original *zip.Reader, replacements map[string][]byte) []byte {
	t.Helper()
	var out, sums bytes.Buffer
	w := zip.NewWriter(&out)
	for _, entry := range original.File {
		r, err := entry.Open()
		require.NoError(t, err)
		body, err := io.ReadAll(r)
		require.NoError(t, err)
		require.NoError(t, r.Close())
		if replacement, ok := replacements[entry.Name]; ok {
			body = replacement
		}
		if entry.Name == "SHA256SUMS" {
			body = sums.Bytes()
		}
		header := &zip.FileHeader{Name: entry.Name, Method: zip.Store, ModifiedDate: 33} //nolint:staticcheck // Match the strict portable ZIP profile independently.
		header.SetMode(0600)
		dst, err := w.CreateHeader(header)
		require.NoError(t, err)
		_, err = dst.Write(body)
		require.NoError(t, err)
		if entry.Name != "SHA256SUMS" {
			hash := sha256.Sum256(body)
			fmt.Fprintf(&sums, "%x  %s\n", hash, entry.Name)
		}
	}
	require.NoError(t, w.Close())
	return out.Bytes()
}
