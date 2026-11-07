//go:build !darwin || arm64

package store

import (
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
	"testing"
)

// An independent boolean oracle checks every component of eligibility. Shared
// bytes alone do not authorize a different version or mismatched source size.
func TestPublishedNoteSelectorEligibilityProperty(t *testing.T) {
	s := newTestStore(t)
	source := seedNotePublication(t, s, "present")
	otherVersion := noteOperationID(t)
	hegel.Test(t, func(ht *hegel.T) {
		mask := hegel.Draw(ht, hegel.Integers(0, 63))
		membership := hegel.Draw(ht, hegel.Integers(0, 2))
		presence := []string{"present", "absent", "unknown"}[membership]
		active, availability, version, hash, size, trash := 1, "retained", source.Identity.ContentVersionID, source.Identity.Hash, source.Identity.Size, any(nil)
		if mask&1 != 0 {
			active = 0
		}
		if mask&2 != 0 {
			availability = "unavailable"
		}
		if mask&4 != 0 {
			version = otherVersion
		}
		if mask&8 != 0 {
			hash = fakeHash("ef")
		}
		if mask&16 != 0 {
			size++
		}
		if mask&32 != 0 {
			trash = nowRFC3339()
		}
		_, err := s.db.ExecContext(ht.Context(), `UPDATE note_publications SET active=?`, active)
		require.NoError(ht, err)
		_, err = s.db.ExecContext(ht.Context(), `UPDATE published_note_sources SET presence=?,availability=?,content_version_id=?,blob_hash=?,size=?`, presence, availability, version, hash, size)
		require.NoError(ht, err)
		_, err = s.db.ExecContext(ht.Context(), `UPDATE nodes SET trashed_at=? WHERE id=?`, trash, source.Identity.NodeID)
		require.NoError(ht, err)
		want := 0
		if mask == 0 && membership == 0 {
			want = 1
		}
		for _, publication := range []string{"", "notebook", "other"} {
			count, err := liveSearchScopeCount(ht.Context(), s.db, SearchOptions{PublishedNotes: &PublishedNoteSelector{Publication: publication}})
			require.NoError(ht, err)
			expected := want
			if publication == "other" {
				expected = 0
			}
			require.Equal(ht, expected, count, "mask=%d membership=%s publication=%s", mask, presence, publication)
		}
	}, hegel.WithDatabase(t.TempDir()), hegel.WithTestCases(2000))
}
