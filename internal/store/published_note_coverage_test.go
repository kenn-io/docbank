package store

import (
	"fmt"
	"hegel.dev/go/hegel"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

// Seed only publication membership; processing heads/jobs use their real APIs.
func publishCoverageVersions(tb testing.TB, s *Store, versions []string) {
	tb.Helper()
	_, err := s.db.ExecContext(tb.Context(), `INSERT INTO note_publications
		(name,destination,policy_fingerprint,namespace_revision,active,last_complete_at,last_accepted_at)
		VALUES('notebook','/',?,1,1,'','') ON CONFLICT(name) DO NOTHING`, fakeHash("ab"))
	require.NoError(tb, err)
	for i, version := range versions {
		_, err = s.db.ExecContext(tb.Context(), `INSERT INTO published_note_sources
			(publication_name,source_ref,node_id,content_version_id,blob_hash,size,source_revision,presence,availability,accepted_at)
			SELECT 'notebook',?,node_id,version_id,blob_hash,size,1,'present','retained',recorded_at
			FROM content_versions WHERE version_id=?`, fmt.Sprintf("source-%05d.md", i), version)
		require.NoError(tb, err)
	}
}

func TestPublishedNoteCoverageScope(t *testing.T) {
	t.Parallel()
	s, version, profile, _ := newEmbeddingCatalogFixture(t)
	publishCoverageVersions(t, s, []string{version})
	exact := ProcessingCoverageScope{ContentVersionIDs: []string{version}, ProcessingProfileFingerprint: profile.Fingerprint}
	want, err := s.ProcessingCoverage(t.Context(), exact)
	require.NoError(t, err)
	for _, publication := range []string{"", "notebook"} {
		got, err := s.ProcessingCoverage(t.Context(), ProcessingCoverageScope{
			PublishedNotes: &PublishedNoteSelector{Publication: publication}, ProcessingProfileFingerprint: profile.Fingerprint,
		})
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
	for _, scope := range []ProcessingCoverageScope{
		{ProcessingProfileFingerprint: profile.Fingerprint},
		{ContentVersionIDs: []string{version}, PublishedNotes: &PublishedNoteSelector{}, ProcessingProfileFingerprint: profile.Fingerprint},
		{PublishedNotes: &PublishedNoteSelector{Publication: "INVALID"}, ProcessingProfileFingerprint: profile.Fingerprint},
		{PublishedNotes: &PublishedNoteSelector{}, ProcessingProfileFingerprint: "invalid"},
		{PublishedNotes: &PublishedNoteSelector{}, ProcessingProfileFingerprint: profile.Fingerprint,
			Bindings: make([]ProcessingCoverageBinding, 65)},
		{PublishedNotes: &PublishedNoteSelector{}, ProcessingProfileFingerprint: profile.Fingerprint,
			Bindings: []ProcessingCoverageBinding{{BindingID: ""}}},
		{PublishedNotes: &PublishedNoteSelector{}, ProcessingProfileFingerprint: profile.Fingerprint,
			Bindings: []ProcessingCoverageBinding{{BindingID: "optional", Required: true}}},
		{PublishedNotes: &PublishedNoteSelector{}, ProcessingProfileFingerprint: profile.Fingerprint,
			Bindings: []ProcessingCoverageBinding{{BindingID: "missing"}}},
		{PublishedNotes: &PublishedNoteSelector{}, ProcessingProfileFingerprint: profile.Fingerprint,
			Bindings: []ProcessingCoverageBinding{{BindingID: "optional"}, {BindingID: "optional"}}},
	} {
		_, err := s.ProcessingCoverage(t.Context(), scope)
		require.Error(t, err, "%+v", scope)
	}
	empty, err := s.ProcessingCoverage(t.Context(), ProcessingCoverageScope{
		PublishedNotes: &PublishedNoteSelector{Publication: "missing"}, ProcessingProfileFingerprint: profile.Fingerprint,
	})
	require.NoError(t, err)
	require.Zero(t, empty.Renditions.Total)
	// Unselected archive coverage must keep its prior exact-version behavior.
	_, err = s.db.ExecContext(t.Context(), `UPDATE published_note_sources SET presence='absent'`)
	require.NoError(t, err)
	got, err := s.ProcessingCoverage(t.Context(), exact)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestPublishedNoteCoverageSelectedReplacement(t *testing.T) {
	t.Parallel()
	s, version, profile, attachment := newEmbeddingCatalogFixture(t)
	other, err := s.CreateFile(t.Context(), s.RootID(), "selected-unprocessed.pdf", catalogSourceHash, 20, "application/pdf")
	require.NoError(t, err)
	distractor, err := s.CreateFile(t.Context(), s.RootID(), "archive-only.pdf", catalogSourceHash, 20, "application/pdf")
	require.NoError(t, err)
	publishCoverageVersions(t, s, []string{version, other.CurrentVersionID})
	for _, id := range []string{version, distractor.CurrentVersionID} {
		set := embeddingSetFixture(t.Context(), s, id, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
		require.NoError(t, s.StageEmbeddingSet(t.Context(), set))
		require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
			FencingToken: 1, Key: EmbeddingHeadKey{ContentVersionID: id, BindingID: set.BindingID, InputKind: set.InputKind},
			SetID: set.ID, VectorSpaceID: set.VectorSpace.ID, ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
		}))
	}
	for _, id := range []string{version, other.CurrentVersionID, distractor.CurrentVersionID} {
		_, err := s.EnqueueEmbeddingJob(t.Context(), embeddingJobTestRequest(t, s, id, profile, "published-rebuild-"+id))
		require.NoError(t, err)
	}
	chunk := embeddingSetFixture(t.Context(), s, version, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachment)
	require.NoError(t, s.StageEmbeddingSet(t.Context(), chunk))
	require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
		FencingToken: 1, Key: EmbeddingHeadKey{ContentVersionID: version, BindingID: chunk.BindingID, InputKind: chunk.InputKind},
		SetID: chunk.ID, VectorSpaceID: chunk.VectorSpace.ID, ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
	}))
	request := renditionJobTestRequest(version, profile)
	grantRenditionJobConsent(t, s, request)
	_, _, err = s.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	got, err := s.ProcessingCoverage(t.Context(), ProcessingCoverageScope{PublishedNotes: &PublishedNoteSelector{},
		ProcessingProfileFingerprint: profile.Fingerprint, Bindings: []ProcessingCoverageBinding{{BindingID: "optional"}, {BindingID: "chunk"}},
	})
	require.NoError(t, err)
	require.Equal(t, ProcessingClassCoverage{Name: "rendition", State: "rebuilding", Total: 2,
		Rebuilding: 1, PreviousGenerationServing: 1, Unavailable: 1}, got.Renditions)
	require.Equal(t, ProcessingClassCoverage{Name: "optional", State: "rebuilding", Total: 2,
		Rebuilding: 2, PreviousGenerationServing: 1}, got.Embeddings[0])
	require.Equal(t, ProcessingClassCoverage{Name: "chunk", State: "partial", Total: 2,
		Complete: 1, Unavailable: 1}, got.Embeddings[1])
	// Once the serving source is absent, it may not contribute to either side
	// of the previous-serving/rebuilding intersection.
	_, err = s.db.ExecContext(t.Context(), `UPDATE published_note_sources SET presence='absent' WHERE content_version_id=?`, version)
	require.NoError(t, err)
	got, err = s.ProcessingCoverage(t.Context(), ProcessingCoverageScope{PublishedNotes: &PublishedNoteSelector{},
		ProcessingProfileFingerprint: profile.Fingerprint, Bindings: []ProcessingCoverageBinding{{BindingID: "optional"}}})
	require.NoError(t, err)
	require.Equal(t, ProcessingClassCoverage{Name: "optional", State: "rebuilding", Total: 1, Rebuilding: 1}, got.Embeddings[0])
}

func TestPublishedNoteCoverageScale4098(t *testing.T) {
	t.Parallel()
	s, version, profile, _ := newEmbeddingCatalogFixture(t)
	publishCoverageVersions(t, s, []string{version})
	tx, err := s.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	for i := 1; i < 4098; i++ {
		id := noteOperationID(t)
		result, err := tx.ExecContext(t.Context(), `INSERT INTO nodes(parent_id,name,kind,current_version_id,revision,created_at,modified_at)
			SELECT parent_id,?,'file',?,1,created_at,modified_at FROM nodes WHERE current_version_id=?`, fmt.Sprintf("scale-%04d.md", i), id, version)
		require.NoError(t, err)
		node, err := result.LastInsertId()
		require.NoError(t, err)
		_, err = tx.ExecContext(t.Context(), `INSERT INTO content_versions(version_id,node_id,blob_hash,size,mime_type,recorded_at,node_revision,introduced_operation_id,transition_kind)
			SELECT ?,?,blob_hash,size,mime_type,recorded_at,1,?,'content_create' FROM content_versions WHERE version_id=?`, id, node, noteOperationID(t), version)
		require.NoError(t, err)
		_, err = tx.ExecContext(t.Context(), `INSERT INTO published_note_sources(publication_name,source_ref,node_id,content_version_id,blob_hash,size,source_revision,presence,availability,accepted_at)
			SELECT 'notebook',?,node_id,version_id,blob_hash,size,1,'present','retained',recorded_at FROM content_versions WHERE version_id=?`, fmt.Sprintf("scale-%04d.md", i), id)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
	got, err := s.ProcessingCoverage(t.Context(), ProcessingCoverageScope{PublishedNotes: &PublishedNoteSelector{}, ProcessingProfileFingerprint: profile.Fingerprint})
	require.NoError(t, err)
	require.Equal(t, ProcessingClassCoverage{Name: "rendition", State: "partial", Total: 4098, Complete: 1, Unavailable: 4097}, got.Renditions)
}

// A corrupt build beyond the first validation page must still fail the snapshot;
// a shared build counts both heads without expanding the validation page.
func TestPublishedNoteCoverageBuildPages(t *testing.T) {
	t.Parallel()
	s, versions := newRenditionCatalogFixture(t)
	profile := catalogProcessingProfile(t, false)
	const groups = 130
	for i := range groups {
		build := catalogRenditionBuild(s, profile)
		build.ID = fmt.Sprintf("%064x", i+1)
		require.NoError(t, s.StageRenditionBuild(t.Context(), build))
		var version string
		if i < len(versions) {
			version = versions[i]
		} else {
			node, err := s.CreateFile(t.Context(), s.RootID(), fmt.Sprintf("build-%03d.pdf", i), catalogSourceHash, 20, "application/pdf")
			require.NoError(t, err)
			version = node.CurrentVersionID
			versions = append(versions, version)
		}
		require.NoError(t, publishRenditionForTest(t, s, RenditionAttachmentRecord{
			ID: fmt.Sprintf("%064x", i+1000), VaultID: s.VaultID(), ContentVersionID: version,
			BuildID: build.ID, Profile: profile, AttachedAt: embeddingCatalogTime,
		}, embeddingCatalogTime, testSHA256([]byte(build.ID+"-lexical"))))
	}
	shared, err := s.CreateFile(t.Context(), s.RootID(), "shared-build.pdf", catalogSourceHash, 20, "application/pdf")
	require.NoError(t, err)
	require.NoError(t, publishRenditionForTest(t, s, RenditionAttachmentRecord{
		ID: fmt.Sprintf("%064x", 2000), VaultID: s.VaultID(), ContentVersionID: shared.CurrentVersionID,
		BuildID: fmt.Sprintf("%064x", 1), Profile: profile, AttachedAt: embeddingCatalogTime,
	}, embeddingCatalogTime, testSHA256([]byte(fmt.Sprintf("%064x", 1)+"-lexical"))))
	versions = append(versions, shared.CurrentVersionID)
	publishCoverageVersions(t, s, versions)
	scope := ProcessingCoverageScope{PublishedNotes: &PublishedNoteSelector{Publication: "notebook"}, ProcessingProfileFingerprint: profile.Fingerprint}
	got, err := s.ProcessingCoverage(t.Context(), scope)
	require.NoError(t, err)
	require.Equal(t, ProcessingClassCoverage{Name: "rendition", State: "complete", Complete: groups + 1, Total: groups + 1}, got.Renditions)
	_, err = s.db.ExecContext(t.Context(), `DROP TRIGGER rendition_lexical_segments_immutable_update`)
	require.NoError(t, err)
	_, err = s.db.ExecContext(t.Context(), `UPDATE rendition_lexical_segments SET char_end=999 WHERE build_id=?`, fmt.Sprintf("%064x", groups))
	require.NoError(t, err)
	_, err = s.ProcessingCoverage(t.Context(), scope)
	require.ErrorContains(t, err, "rendition lexical segment 0 is invalid")
}

// The oracle derives eligibility from independent generated source facts. A
// missing SQL authority clause changes totals and the exact serving/rebuild
// intersection, even when those excluded heads still exist in the archive.
func TestPublishedNoteCoverageSourceStateProperty(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "darwin" && runtime.GOARCH != "arm64" {
		t.Skip("Hegel native engine requires macOS arm64")
	}
	s, first, profile, _ := newEmbeddingCatalogFixture(t)
	versions := []string{first}
	for i := 1; i < 6; i++ {
		node, err := s.CreateFile(t.Context(), s.RootID(), fmt.Sprintf("oracle-%d.pdf", i), catalogSourceHash, 20, "application/pdf")
		require.NoError(t, err)
		versions = append(versions, node.CurrentVersionID)
	}
	publishCoverageVersions(t, s, versions)
	_, err := s.db.ExecContext(t.Context(), `INSERT INTO note_publications
        (name,destination,policy_fingerprint,namespace_revision,active,last_complete_at,last_accepted_at)
        VALUES('work','/work',?,1,1,'','')`, fakeHash("bc"))
	require.NoError(t, err)
	for i, id := range versions {
		if i >= 3 {
			_, err = s.db.ExecContext(t.Context(), `UPDATE published_note_sources SET publication_name='work' WHERE content_version_id=?`, id)
			require.NoError(t, err)
		}
	}
	for i, id := range versions {
		if i%2 == 0 {
			set := embeddingSetFixture(t.Context(), s, id, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
			require.NoError(t, s.StageEmbeddingSet(t.Context(), set))
			require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{FencingToken: 1,
				Key:   EmbeddingHeadKey{ContentVersionID: id, BindingID: set.BindingID, InputKind: set.InputKind},
				SetID: set.ID, VectorSpaceID: set.VectorSpace.ID, ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime}))
		}
		if i%3 == 0 {
			_, err := s.EnqueueEmbeddingJob(t.Context(), embeddingJobTestRequest(t, s, id, profile, "oracle-"+id))
			require.NoError(t, err)
		}
	}
	hegel.Test(t, func(ht *hegel.T) {
		active := []bool{hegel.Draw(ht, hegel.Booleans()), hegel.Draw(ht, hegel.Booleans())}
		selector := PublishedNoteSelector{Publication: hegel.Draw(ht, hegel.SampledFrom([]string{"", "notebook", "work", "missing"}))}
		tx, err := s.db.BeginTx(t.Context(), nil)
		require.NoError(ht, err)
		defer func() { _ = tx.Rollback() }()
		_, err = tx.ExecContext(t.Context(), `UPDATE note_publications SET active=? WHERE name='notebook'`, active[0])
		require.NoError(ht, err)
		_, err = tx.ExecContext(t.Context(), `UPDATE note_publications SET active=? WHERE name='work'`, active[1])
		require.NoError(ht, err)
		wantR := ProcessingClassCoverage{Name: "rendition"}
		wantE := ProcessingClassCoverage{Name: "optional"}
		for i, id := range versions {
			// Each fault removes an otherwise exact source from this population.
			fault := hegel.Draw(ht, hegel.Integers(0, 7))
			presence, availability, hash, sourceVersion, size := "present", "retained", catalogSourceHash, id, 20
			var trash any
			switch fault {
			case 1:
				presence = "absent"
			case 2:
				availability = "pruned"
			case 3:
				hash = fakeHash("fd")
			case 4:
				size++
			case 5:
				sourceVersion = "00000000-0000-4000-8000-000000000099"
			case 6:
				trash = embeddingCatalogTime
			}
			_, err = tx.ExecContext(t.Context(), `UPDATE published_note_sources SET presence=?,availability=?,blob_hash=?,size=?,content_version_id=? WHERE node_id=(SELECT node_id FROM content_versions WHERE version_id=?)`, presence, availability, hash, size, sourceVersion, id)
			require.NoError(ht, err)
			_, err = tx.ExecContext(t.Context(), `UPDATE nodes SET trashed_at=? WHERE current_version_id=?`, trash, id)
			require.NoError(ht, err)
			publication := []string{"notebook", "work"}[i/3]
			if !active[i/3] || (selector.Publication != "" && selector.Publication != publication) || (fault != 0 && fault != 7) {
				continue
			}
			wantR.Total++
			wantE.Total++
			if i == 0 {
				wantR.Complete++
			} else {
				wantR.Unavailable++
			}
			if i%3 == 0 {
				wantE.Rebuilding++
				if i%2 == 0 {
					wantE.PreviousGenerationServing++
				}
			} else if i%2 == 0 {
				wantE.Complete++
			} else {
				wantE.Unavailable++
			}
		}
		require.NoError(ht, tx.Commit())
		// State oracle uses ranked expected categories, independently of the
		// production function that consumes the SQL aggregates.
		for _, item := range []*ProcessingClassCoverage{&wantR, &wantE} {
			item.State = "unavailable"
			if item.Complete > 0 {
				item.State = "partial"
			}
			if item.Complete == item.Total {
				item.State = "complete"
			}
			if item.Rebuilding > 0 {
				item.State = "rebuilding"
			}
			if item.Total == 0 {
				item.State = "stale"
			}
		}
		got, err := s.ProcessingCoverage(t.Context(), ProcessingCoverageScope{PublishedNotes: &selector,
			ProcessingProfileFingerprint: profile.Fingerprint, Bindings: []ProcessingCoverageBinding{{BindingID: "optional"}}})
		require.NoError(ht, err)
		require.Equal(ht, wantR, got.Renditions)
		require.Equal(ht, []ProcessingClassCoverage{wantE}, got.Embeddings)
	}, hegel.WithDatabase(t.TempDir()), hegel.WithTestCases(2000))
}

func TestPublishedNoteCoverageChunkAuthority(t *testing.T) {
	t.Parallel()
	s, version, profile, attachment := newEmbeddingCatalogFixture(t)
	publishCoverageVersions(t, s, []string{version})
	chunk := embeddingSetFixture(t.Context(), s, version, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachment)
	require.NoError(t, s.StageEmbeddingSet(t.Context(), chunk))
	require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{FencingToken: 1,
		Key:   EmbeddingHeadKey{ContentVersionID: version, BindingID: chunk.BindingID, InputKind: chunk.InputKind},
		SetID: chunk.ID, VectorSpaceID: chunk.VectorSpace.ID, ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime}))
	scope := ProcessingCoverageScope{PublishedNotes: &PublishedNoteSelector{}, ProcessingProfileFingerprint: profile.Fingerprint,
		Bindings: []ProcessingCoverageBinding{{BindingID: "chunk"}}}
	got, err := s.ProcessingCoverage(t.Context(), scope)
	require.NoError(t, err)
	require.Equal(t, ProcessingClassCoverage{Name: "chunk", State: "complete", Total: 1, Complete: 1}, got.Embeddings[0])
	_, err = s.db.ExecContext(t.Context(), `DROP TRIGGER rendition_builds_immutable_update`)
	require.NoError(t, err)
	_, err = s.db.ExecContext(t.Context(), `UPDATE rendition_builds SET evidence_checksum=? WHERE build_id=(SELECT build_id FROM rendition_attachments WHERE attachment_id=?)`, fakeHash("ec"), attachment)
	require.NoError(t, err)
	got, err = s.ProcessingCoverage(t.Context(), scope)
	require.NoError(t, err)
	require.Equal(t, ProcessingClassCoverage{Name: "chunk", State: "unavailable", Total: 1, Unavailable: 1}, got.Embeddings[0])
}

func TestPublishedNoteCoverageImageJobAuthority(t *testing.T) {
	t.Parallel()
	f := newImageEmbeddingCatalogFixture(t)
	publishCoverageVersions(t, f.s, []string{f.node.CurrentVersionID})
	_, err := f.s.EnqueueEmbeddingJob(t.Context(), f.request)
	require.NoError(t, err)
	scope := ProcessingCoverageScope{PublishedNotes: &PublishedNoteSelector{}, ProcessingProfileFingerprint: f.request.Profile.Fingerprint,
		Bindings: []ProcessingCoverageBinding{{BindingID: f.request.BindingID}}}
	got, err := f.s.ProcessingCoverage(t.Context(), scope)
	require.NoError(t, err)
	require.Equal(t, ProcessingClassCoverage{Name: f.request.BindingID, State: "rebuilding", Total: 1, Rebuilding: 1}, got.Embeddings[0])
	// Retaining the queued job is insufficient after its preview evidence loses
	// authority. Source membership alone must not count it as rebuilding.
	_, err = f.s.db.ExecContext(t.Context(), `DELETE FROM visual_preview_generations WHERE content_version_id=?`, f.node.CurrentVersionID)
	require.NoError(t, err)
	got, err = f.s.ProcessingCoverage(t.Context(), scope)
	require.NoError(t, err)
	require.Equal(t, ProcessingClassCoverage{Name: f.request.BindingID, State: "unavailable", Total: 1, Unavailable: 1}, got.Embeddings[0])
}
