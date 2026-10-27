package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

type photoStackCountingQuerier struct {
	*sql.DB

	generationReads int
}

func (q *photoStackCountingQuerier) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if strings.Contains(query, "generation_bytes") {
		q.generationReads++
	}
	return q.DB.QueryRowContext(ctx, query, args...)
}

func TestPhotoStackCorruptGenerationFallsBackToBurstGap(t *testing.T) {
	t.Parallel()
	f := newImageEmbeddingCatalogFixture(t)
	record, vector := f.publish(t)
	index := newCursorIndexFixture(f)
	index.add(record, vector)
	index.generation(t, "corrupt-image-generation")
	var data []byte
	require.NoError(t, f.s.db.QueryRow(`SELECT generation_bytes FROM vector_index_generations WHERE generation_id=?`, browseHash("corrupt-image-generation")).Scan(&data))
	data[0] ^= 0xff
	_, err := f.s.db.Exec(`UPDATE vector_index_generations SET generation_bytes=? WHERE generation_id=?`, data, browseHash("corrupt-image-generation"))
	require.NoError(t, err)
	ids := burstPhotos(t, f.s, 2)
	page, err := f.s.ListPhotoAssets(t.Context(), captureRequest(t, "asc"), nil)
	require.NoError(t, err)
	require.Contains(t, stackAssetIDs(page.Stacks), ids)
}

func TestPhotoStackBackwardWalkBoundsImageCandidates(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		seconds, count int
		indexed        bool
	}{
		{"dense burst", 1, MaxPhotoStackFrames*2 + 20, false},
		{"unindexed image window", 30, MaxPhotoStackFrames*2 + 20, false},
		{"indexed image window", 30, MaxPhotoStackFrames + 20, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newImageEmbeddingCatalogFixture(t)
			record, vector := f.publish(t)
			index := newCursorIndexFixture(f)
			index.add(record, vector)
			index.generation(t, "unrelated-image-generation")
			run, err := f.s.BeginIngest(t.Context(), "cli", "synthetic long camera stream")
			require.NoError(t, err)
			var last string
			for i := range tc.count {
				stamp := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i*tc.seconds) * time.Second).Format(time.RFC3339)
				last = stackPhoto(t, f.s, run, fmt.Sprintf("/long/frame-%04d.jpg", i), stamp, document.SourceMetadataPrecisionSecond)
				if tc.indexed {
					index.publish(t, last, []float64{1, 0, 0, 0, 0, 0, 0, 0})
				}
			}
			if tc.indexed {
				index.generation(t, "indexed-image-window")
			}
			if tc.seconds == 1 {
				index.publish(t, last, []float64{1, 0, 0, 0, 0, 0, 0, 0})
				index.generation(t, "indexed-long-stream")
			}
			q := &photoStackCountingQuerier{DB: f.s.db}
			vectors, err := f.s.newPhotoStackVectorReader(t.Context(), q)
			require.NoError(t, err)
			frames, err := f.s.photoStackFrames(t.Context(), q, []string{last}, false, "")
			require.NoError(t, err)
			pager, err := newPhotoStackPager(t.Context(), q, photoBrowseScope{match: compiledQueryFragment{sql: "1=1"}}, 2, true, "", vectors)
			require.NoError(t, err)
			defer func() { require.NoError(t, pager.Close()) }()
			hydrateCalls := 0
			vectors.onHydrate = func(int) { hydrateCalls++ }
			component := &photoStackComponent{frames: []photoStackFrame{frames[last]}}
			require.NoError(t, pager.walk(t.Context(), frames[last], false, component))
			require.LessOrEqual(t, hydrateCalls, 1, "batch image candidates before hydration")
		})
	}
}

func TestPhotoStackPagesReuseOpenedGeneration(t *testing.T) {
	t.Parallel()
	f := newImageEmbeddingCatalogFixture(t)
	record, vector := f.publish(t)
	index := newCursorIndexFixture(f)
	index.add(record, vector)
	index.generation(t, "reused-image-generation")
	burstPhotos(t, f.s, 4)
	run, err := f.s.BeginIngest(t.Context(), "cli", "synthetic separate page")
	require.NoError(t, err)
	stackPhoto(t, f.s, run, "/separate/one.jpg", "2024-06-10T13:00:00Z", document.SourceMetadataPrecisionSecond)
	stackPhoto(t, f.s, run, "/separate/two.jpg", "2024-06-10T14:00:00Z", document.SourceMetadataPrecisionSecond)
	q := &photoStackCountingQuerier{DB: f.s.db}
	var boundary *PhotoBrowsePosition
	for i := range 2 {
		page := PhotoBrowsePage{}
		err := f.s.listPhotoStackPage(t.Context(), q, photoBrowseScope{match: compiledQueryFragment{sql: "1=1"}}, 2, false, 1, boundary, "", func(after *PhotoBrowsePosition, limit int) ([]PhotoBrowseRow, error) {
			statement := `SELECT ` + photoBrowseSelect + `,pl.sort_key,0 FROM ` + photoBrowseDisplayFrom + ` WHERE ` + photoBrowseLiveDisplay + ` AND ` + photoVisibilityPredicate(false) + ` AND pl.sort_key>''`
			var args []any
			if after != nil {
				statement += ` AND (pl.sort_key>? OR (pl.sort_key=? AND a.asset_id>?))`
				args = append(args, after.Key, after.Key, after.AssetID)
			}
			args = append(args, limit)
			rows, err := q.QueryContext(t.Context(), statement+` ORDER BY pl.sort_key,a.asset_id LIMIT ?`, args...)
			if err != nil {
				return nil, err
			}
			return scanPhotoBrowseRows(rows, PhotoBrowsePosition{})
		}, &page, "")
		require.NoError(t, err)
		require.NotEmpty(t, page.Items)
		boundary = page.Next
		if i == 0 {
			require.NotNil(t, boundary)
		}
	}
	require.Equal(t, 1, q.generationReads, "the second page must reuse the immutable generation")
	index.generation(t, "replacement-image-generation")
	_, err = f.s.newPhotoStackVectorReader(t.Context(), q)
	require.NoError(t, err)
	require.Equal(t, 2, q.generationReads)
	require.Len(t, f.s.photoStackVectors, 1, "retain only the current generation per space")
	require.NoError(t, f.s.Close())
	require.Empty(t, f.s.photoStackVectors)
}

func TestPhotoStackDecisionRejectsGenerationPublishedBeforeTransaction(t *testing.T) {
	t.Parallel()
	f := newImageEmbeddingCatalogFixture(t)
	ids := burstPhotos(t, f.s, 2)
	index := newCursorIndexFixture(f)
	for _, id := range ids {
		index.publish(t, id, []float64{1, 0, 0, 0, 0, 0, 0, 0})
	}
	index.generation(t, "decision-before")
	targets := preparedPhotoTargets(t.Context(), t, f.s, PhotoSetSelection{AssetIDs: ids, Limit: MaxPhotoStackTargets})
	vectors, err := f.s.newPhotoStackVectorReader(t.Context(), f.s.db)
	require.NoError(t, err)
	before, err := f.s.LookupPhotoFlags(t.Context(), ids)
	require.NoError(t, err)
	index.generation(t, "decision-after")
	_, err = f.s.decidePhotoStack(t.Context(), PhotoStackDecision{BurstGapSeconds: 2, AssetIDs: ids, Targets: targets, Decision: "current", CurrentID: ids[0]}, vectors)
	require.ErrorIs(t, err, ErrStalePhotoStack)
	after, err := f.s.LookupPhotoFlags(t.Context(), ids)
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestPhotoStackVectorsUseOnlyCosineCompatibleSpaces(t *testing.T) {
	t.Parallel()
	for _, metric := range []string{document.VectorMetricCosine, document.VectorMetricDotProduct, document.VectorMetricL2} {
		t.Run(metric, func(t *testing.T) {
			f := newImageEmbeddingCatalogFixture(t)
			f.request.Descriptor.Metric = metric
			var err error
			f.request.Descriptor, err = document.NewEmbeddingDescriptor(f.request.Descriptor)
			require.NoError(t, err)
			var profile document.ProcessingProfileV1
			require.NoError(t, json.Unmarshal(f.request.Profile.CanonicalProfile, &profile))
			for i := range profile.Embeddings {
				if profile.Embeddings[i].Name == f.binding.Name {
					profile.Embeddings[i].Metric = metric
					profile.Embeddings[i].Descriptor.Fingerprint = f.request.Descriptor.Fingerprint
					f.binding = profile.Embeddings[i]
				}
			}
			canonical, fingerprints, err := document.CanonicalProfile(profile)
			require.NoError(t, err)
			f.request.Profile.CanonicalProfile = canonical
			f.request.Profile.Fingerprint = fingerprints.Profile
			f.request.Profile.RenditionRequestFingerprint = fingerprints.RenditionRequest
			f.request.Profile.EvidenceLexicalFingerprint = fingerprints.EvidenceLexical
			f.request.Profile.RetentionDisclosureFingerprint = fingerprints.RetentionDisclosure
			f.request.Authorization.ProfileFingerprint = fingerprints.Profile
			f.request.InputGeneration.ProcessingProfileFingerprint = fingerprints.Profile
			_, err = f.s.GrantConsent(t.Context(), ProcessingConsentGrantRequest{Principal: f.request.Authorization.Principal, Scope: f.request.Authorization.Scope, ProfileFingerprint: fingerprints.Profile, DisclosureFingerprint: f.binding.DisclosureFingerprint, InputClasses: f.request.Authorization.InputClasses, RetainedArtifactClasses: f.request.Authorization.RetainedArtifactClasses})
			require.NoError(t, err)
			run, err := f.s.BeginIngest(t.Context(), "cli", "synthetic metric stacks")
			require.NoError(t, err)
			ids := []string{
				stackPhoto(t, f.s, run, "/metric/one.jpg", "2024-06-10T12:00:00Z", document.SourceMetadataPrecisionSecond),
				stackPhoto(t, f.s, run, "/metric/two.jpg", "2024-06-10T12:00:30Z", document.SourceMetadataPrecisionSecond),
			}
			index := newCursorIndexFixture(f)
			for _, id := range ids {
				index.publish(t, id, []float64{1, 0, 0, 0, 0, 0, 0, 0})
			}
			index.generation(t, "metric-generation")
			groups, _, _, _, err := f.s.PhotoStackGroups(t.Context(), ids, false, "")
			require.NoError(t, err)
			if metric == document.VectorMetricL2 {
				require.Empty(t, groups)
			} else {
				require.Equal(t, [][]string{ids}, groups)
			}
		})
	}
}
