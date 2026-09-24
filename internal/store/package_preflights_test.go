package store

import (
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPackagePreflightReadExpiresBeforeCleanup(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := newTestStore(t)
		record := putPreflight(t, s, hoursFromNow(t, 0), hoursFromNow(t, 24))
		time.Sleep(24*time.Hour - time.Nanosecond)
		_, err := s.PackagePreflight(t.Context(), record.Owner, record.PreflightID)
		require.NoError(t, err)
		time.Sleep(time.Nanosecond)
		_, err = s.PackagePreflight(t.Context(), record.Owner, record.PreflightID)
		require.ErrorIs(t, err, ErrNotFound)
		var retained int
		require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM package_preflights WHERE preflight_id=?`, record.PreflightID).Scan(&retained))
		require.Equal(t, 1, retained, "expiry must be enforced before cleanup deletes the row")
	})
}

func TestPackagePreflightExpiryRemovesOnlyExpiredControlRows(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	expired := PackagePreflightRecord{
		PreflightID: mustPackagePreflightID(t), Owner: "synthetic",
		SourceKind: "root", SourceRef: "synthetic-root-handle",
		ProfileSHA256: strings.Repeat("a", 64), MappingSHA256: strings.Repeat("b", 64),
		ManifestSHA256: strings.Repeat("c", 64), ManifestBlobSHA256: strings.Repeat("c", 64),
		CanonicalJSON: []byte("{}"), DiagnosticsJSON: []byte("[]"),
		CreatedAt: "2026-09-10T00:00:00.000000000Z", ExpiresAt: "2026-09-11T00:00:00.000000000Z",
	}
	_, err := s.PutPackagePreflight(t.Context(), expired)
	require.NoError(t, err)
	n, err := s.ExpirePackagePreflights(t.Context(), "2026-09-12T00:00:00.000000000Z")
	require.NoError(t, err)
	require.Equal(t, int64(1), n)
	_, err = s.PackagePreflight(t.Context(), expired.Owner, expired.PreflightID)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestExpiredPreflightsAreSweptAndUnexpiredOnesSurvive(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	fresh := putPreflight(t, s, hoursFromNow(t, 0), hoursFromNow(t, 24))
	stale := putPreflight(t, s, hoursFromNow(t, -48), hoursFromNow(t, -24))
	removed, err := s.ExpirePackagePreflights(t.Context(), hoursFromNow(t, 0))
	require.NoError(t, err)
	assert.Equal(t, int64(1), removed)
	_, err = s.PackagePreflight(t.Context(), stale.Owner, stale.PreflightID)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.PackagePreflight(t.Context(), fresh.Owner, fresh.PreflightID)
	require.NoError(t, err)
}

func TestPackagePreflightBlobStagingIsAGCHold(t *testing.T) {
	t.Parallel()
	want := map[string]bool{"manifest_blob_sha256": false, "diagnostics_blob_sha256": false}
	for _, reference := range blobGCHolds {
		if reference.table == "package_preflights" {
			want[reference.column] = true
		}
	}
	assert.Equal(t, map[string]bool{"manifest_blob_sha256": true, "diagnostics_blob_sha256": true}, want)
}

func TestPackagePreflightRequiresExactManifestBlobIdentity(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	record := PackagePreflightRecord{
		PreflightID: mustPackagePreflightID(t), Owner: "synthetic", SourceKind: "root", SourceRef: "synthetic-root",
		ProfileSHA256: strings.Repeat("a", 64), MappingSHA256: strings.Repeat("b", 64),
		ManifestSHA256: strings.Repeat("c", 64), ManifestBlobSHA256: strings.Repeat("d", 64),
		CanonicalJSON: []byte("{}"), DiagnosticsJSON: []byte("[]"),
	}
	_, err := s.PutPackagePreflight(t.Context(), record)
	require.ErrorContains(t, err, "manifest blob must match")
}

func putPreflight(t *testing.T, s *Store, createdAt, expiresAt string) PackagePreflightRecord {
	t.Helper()
	id := mustPackagePreflightID(t)
	record := PackagePreflightRecord{
		PreflightID: id, Owner: "synthetic", SourceKind: "root", SourceRef: "root-" + id,
		ProfileSHA256: strings.Repeat("a", 64), MappingSHA256: strings.Repeat("b", 64),
		ManifestSHA256: strings.Repeat("c", 64), ManifestBlobSHA256: strings.Repeat("c", 64),
		CanonicalJSON: []byte("{}"), DiagnosticsJSON: []byte("[]"),
		CreatedAt: createdAt, ExpiresAt: expiresAt,
	}
	stored, err := s.PutPackagePreflight(t.Context(), record)
	require.NoError(t, err)
	return stored
}

func hoursFromNow(t *testing.T, delta int) string {
	t.Helper()
	return time.Now().UTC().Add(time.Duration(delta) * time.Hour).Format(timestampLayout)
}

func mustPackagePreflightID(t *testing.T) string {
	t.Helper()
	id, err := newUUIDv4()
	require.NoError(t, err)
	return id
}
