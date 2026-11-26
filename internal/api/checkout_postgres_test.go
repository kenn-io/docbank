package api

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/pgstoretest"
	"go.kenn.io/docbank/internal/store"
)

func postgresCheckoutService(t *testing.T) (PhotoCheckoutService, *sql.DB) {
	t.Helper()
	setup, cfg, runtime := pgstoretest.NewDatabase(t)
	vault := t.TempDir()
	catalog, err := store.OpenPostgres(t.Context(), runtime, cfg, vault)
	require.NoError(t, err)
	return checkoutServiceWithStore(t, vault, catalog), setup
}

func TestPhotoCheckoutWorkersPostgres(t *testing.T) {
	t.Parallel()
	open := func(t *testing.T) PhotoCheckoutService {
		t.Helper()
		s, _ := postgresCheckoutService(t)
		return s
	}
	for _, scenario := range []struct {
		name  string
		check func(*testing.T, func(*testing.T) PhotoCheckoutService)
	}{
		{"RejectsReplacedQueuedDestination", checkCheckoutRunRejectsReplacedQueuedDestination},
		{"RetiredCopiesProtectDestinationUntilCleared", checkCheckoutRetiredCopiesProtectDestinationUntilCleared},
		{"ScanDetectsByteAndIdentityChanges", checkCheckoutScanDetectsByteAndIdentityChanges},
		{"SidecarCommitKeepsNodeAndPairing", checkCheckoutSidecarCommitKeepsNodeAndPairing},
		{"CommitResumeAfterPublication", checkCheckoutCommitResumeAfterPublication},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			scenario.check(t, open)
		})
	}
}

func TestCheckoutRecoversUnrecordedWorkingCopyPostgres(t *testing.T) {
	t.Parallel()
	s, setup := postgresCheckoutService(t)
	ctx := t.Context()
	asset := checkoutPhoto(t, s, "photo.jpg", "original bytes")
	c, op, _, root, err := s.Prepare(ctx, store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}, t.TempDir(), 0)
	require.NoError(t, err)
	defer func() { require.NoError(t, root.Close()) }()
	entries, err := s.Store.PhotoCheckoutEntries(ctx, c.ID)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	path := filepath.FromSlash(entries[0].Path)
	_, err = setup.ExecContext(ctx, `
		CREATE FUNCTION documents.fail_checkout_record() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			RAISE EXCEPTION 'synthetic publication-record failure';
		END $$;
		CREATE TRIGGER fail_checkout_record BEFORE UPDATE OF published ON documents.photo_checkout_entries
		FOR EACH ROW WHEN (NEW.published = 1) EXECUTE FUNCTION documents.fail_checkout_record()`)
	require.NoError(t, err)

	err = s.Run(ctx, c, root)
	require.ErrorIs(t, err, store.ErrPhotoCheckoutPublicationUnrecorded)
	require.ErrorContains(t, err, "synthetic publication-record failure")
	status, err := s.Store.PhotoCheckoutStatus(ctx, c.ID, "", 100)
	require.NoError(t, err)
	require.Zero(t, status.PublishedFiles)
	content, err := root.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "original bytes", string(content))

	// The user can edit the copy while the worker is stopped.
	require.NoError(t, root.WriteFile(path, []byte("edited working copy"), 0o600))
	_, err = setup.ExecContext(ctx, `DROP TRIGGER fail_checkout_record ON documents.photo_checkout_entries`)
	require.NoError(t, err)
	require.NoError(t, s.reconcile(ctx, c))
	status, err = s.Store.PhotoCheckoutStatus(ctx, c.ID, "", 100)
	require.NoError(t, err)
	require.Equal(t, int64(1), status.PublishedFiles)
	finished, err := s.Store.StorageOperation(ctx, op.ID)
	require.NoError(t, err)
	require.Equal(t, store.StorageOperationFailed, finished.State)
	require.Contains(t, finished.Error, "interrupted")
	content, err = root.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "edited working copy", string(content))
}
