package store

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/query"
	"go.kenn.io/docbank/internal/store/pgschema"
)

func snapshotTestQuery(t *testing.T, raw string) query.Query {
	t.Helper()
	value, err := query.Parse([]byte(raw))
	require.NoError(t, err)
	return value
}

func independentMemberHash(members []SnapshotMember) string {
	members = slices.Clone(members)
	slices.SortFunc(members, func(left, right SnapshotMember) int {
		if result := cmp.Compare(left.NodeID, right.NodeID); result != 0 {
			return result
		}
		return strings.Compare(left.ContentVersionID, right.ContentVersionID)
	})
	var input strings.Builder
	for _, member := range members {
		_, _ = fmt.Fprintf(&input, "%d:%s\n", member.NodeID, member.ContentVersionID)
	}
	digest := sha256.Sum256([]byte(input.String()))
	return hex.EncodeToString(digest[:])
}

// Reversing the presentation input must not change the identity receipt, and
// lexically sorting decimal node IDs would put 10 before 2.
func TestQuerySnapshotMemberHashUsesNumericIdentityOrder(t *testing.T) {
	t.Parallel()
	members := []SnapshotMember{
		{NodeID: 10, ContentVersionID: "v10"},
		{NodeID: 2, ContentVersionID: "v2"},
	}
	want := sha256.Sum256([]byte("2:v2\n10:v10\n"))
	assert.Equal(t, hex.EncodeToString(want[:]), snapshotMemberHash(members))
	assert.Equal(t, hex.EncodeToString(want[:]), snapshotMemberHash([]SnapshotMember{members[1], members[0]}))
}

// Omitting member identity, source size, or frozen metadata from the selected
// read view makes this fail even if a live search still returns both files.
func checkQuerySnapshotMaterializesFrozenRowsAndReceipts(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	s := open(t)
	ctx := t.Context()
	run := createCollectionRun(t, s, "zeta.TXT", "snapshot-zeta")
	zeta, err := s.NodeByPath(ctx, "/zeta.TXT")
	require.NoError(t, err)
	archive, err := s.Mkdir(ctx, s.RootID(), "archive")
	require.NoError(t, err)
	directory, err := s.Mkdir(ctx, archive.ID, "nested")
	require.NoError(t, err)
	alpha, _, err := s.IngestFile(ctx, run, directory.ID, "alpha.pdf", fakeHash("snapshot-alpha"),
		17, "application/pdf", "/synthetic/alpha.pdf", "")
	require.NoError(t, err)
	tag, err := s.CreateTag(ctx, "review")
	require.NoError(t, err)
	assigned, err := s.AssignTag(ctx, tag.ID, alpha.ID, alpha.Revision)
	require.NoError(t, err)
	alpha = assigned.Node

	value := snapshotTestQuery(t, `{}`)
	projection, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value})
	require.NoError(t, err)
	require.Len(t, projection.Rows, 2)
	assert.Equal(t, []string{"alpha.pdf", "zeta.TXT"}, []string{
		projection.Rows[0].Name, projection.Rows[1].Name,
	})
	assert.Equal(t, "/archive/nested/alpha.pdf", projection.Rows[0].Path)
	assert.Equal(t, "/zeta.TXT", projection.Rows[1].Path)
	assert.Equal(t, "document", projection.Rows[0].MediaFamily)
	assert.Equal(t, "pdf", query.FilenameExtension(projection.Rows[0].Name))
	assert.Equal(t, []SnapshotTag{{ID: assigned.Tag.ID, Name: assigned.Tag.Name, Revision: assigned.Tag.Revision}}, projection.Rows[0].Tags)
	assert.Equal(t, []SnapshotTag{}, projection.Rows[1].Tags)
	assert.Equal(t, []string{run.ID()}, projection.Rows[0].CollectionIDs)
	assert.Equal(t, run.ID(), projection.Rows[0].DisplayCollectionID)
	assert.Nil(t, projection.Rows[0].DisplayCollectionLabel)
	assert.Equal(t, int64(2), projection.Total)
	assert.Equal(t, alpha.Size+zeta.Size, projection.TotalBytes)
	assert.Equal(t, "sha256:5e60ffa8c5c733830f8dddba716f9bfce8a17121b5f8eb4de97cf23cf2fbe460", projection.QueryFingerprint)
	assert.Equal(t, SnapshotGeneration{Kind: "native"}, projection.Generation)
	assert.Equal(t, 100, projection.PageSize)
	assert.Positive(t, projection.SerializedBytes)
	assert.True(t, strings.HasPrefix(projection.SnapshotFingerprint, "sha256:"))

	members := []SnapshotMember{projection.Rows[0].SnapshotMember, projection.Rows[1].SnapshotMember}
	assert.Equal(t, independentMemberHash(members), projection.MemberHash)

	originalRows := projection.Rows
	originalHash := projection.MemberHash
	originalSnapshotFingerprint := projection.SnapshotFingerprint
	lateTag, err := s.CreateTag(ctx, "late")
	require.NoError(t, err)
	_, err = s.AssignTag(ctx, lateTag.ID, zeta.ID, zeta.Revision)
	require.NoError(t, err)
	assert.Equal(t, originalRows, projection.Rows)
	assert.Equal(t, originalHash, projection.MemberHash)
	assert.Equal(t, originalSnapshotFingerprint, projection.SnapshotFingerprint)
	refreshed, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value})
	require.NoError(t, err)
	assert.Equal(t, originalHash, refreshed.MemberHash, "mutable metadata is not content membership identity")
	assert.NotEqual(t, originalSnapshotFingerprint, refreshed.SnapshotFingerprint,
		"a new frozen projection records the changed tag and node revision")
}

func TestQuerySnapshotRejectsBoundsDuringMaterialization(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		_, err := s.CreateFile(ctx, s.RootID(), name, fakeHash("snapshot-"+name), 1, "text/plain")
		require.NoError(t, err)
	}
	value := snapshotTestQuery(t, `{}`)

	_, err := s.materializeQuerySnapshot(ctx, SnapshotRequest{Query: value}, snapshotMaterializeOptions{MaxRows: 2})
	require.ErrorIs(t, err, ErrQuerySnapshotTooLarge)
	_, err = s.materializeQuerySnapshot(ctx, SnapshotRequest{Query: value}, snapshotMaterializeOptions{MaxSerializedBytes: 1})
	require.ErrorIs(t, err, ErrQuerySnapshotTooLarge)

	rowCharges := 0
	_, err = s.materializeQuerySnapshot(ctx, SnapshotRequest{Query: value}, snapshotMaterializeOptions{
		Charge: func(rows, bytes int64) error {
			if rows > 0 {
				rowCharges++
			}
			if rowCharges == 2 {
				return assert.AnError
			}
			return nil
		},
	})
	require.ErrorIs(t, err, assert.AnError)
	assert.Equal(t, 2, rowCharges, "admission must interrupt row retention instead of checking after full allocation")
}

func TestQuerySnapshotSortsTypedPrimaryAndAscendingIdentityTies(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	first, err := s.CreateFile(ctx, s.RootID(), "z.txt", fakeHash("sort-first"), 10, "TEXT/PLAIN; Charset=UTF-8")
	require.NoError(t, err)
	second, err := s.CreateFile(ctx, s.RootID(), "a.pdf", fakeHash("sort-second"), 2, "application/pdf")
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `UPDATE nodes SET modified_at=? WHERE id=?`, "2026-01-02T00:00:00Z", first.ID)
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `UPDATE nodes SET modified_at=? WHERE id=?`, "2026-01-01T00:00:00Z", second.ID)
	require.NoError(t, err)

	tests := []struct {
		field, direction string
		want             []int64
		keys             []string
	}{
		{"name", "desc", []int64{first.ID, second.ID}, []string{"z.txt", "a.pdf"}},
		{"size", "asc", []int64{second.ID, first.ID}, []string{"2", "10"}},
		{"modified_at", "asc", []int64{second.ID, first.ID}, []string{"2026-01-01T00:00:00.000000000Z", "2026-01-02T00:00:00.000000000Z"}},
		{"media_type", "asc", []int64{second.ID, first.ID}, []string{"application/pdf", "text/plain"}},
	}
	for _, testCase := range tests {
		t.Run(testCase.field, func(t *testing.T) {
			value := snapshotTestQuery(t, fmt.Sprintf(`{"sort":{"field":%q,"direction":%q}}`, testCase.field, testCase.direction))
			projection, err := s.MaterializeQuerySnapshot(t.Context(), SnapshotRequest{Query: value})
			require.NoError(t, err)
			assert.Equal(t, testCase.want, []int64{projection.Rows[0].NodeID, projection.Rows[1].NodeID})
			assert.Equal(t, []string{projection.Rows[0].SortKey, projection.Rows[1].SortKey}, testCase.keys)
		})
	}

	second, _, err = s.ReplaceContent(ctx, second.ID, second.Revision, fakeHash("sort-second-tie"), 10, second.MimeType)
	require.NoError(t, err)
	value := snapshotTestQuery(t, `{"sort":{"field":"size","direction":"desc"}}`)
	projection, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value})
	require.NoError(t, err)
	assert.Equal(t, []int64{first.ID, second.ID}, []int64{projection.Rows[0].NodeID, projection.Rows[1].NodeID},
		"identity ties stay ascending even when the primary direction is descending")
}

func checkDocumentSnapshotSavedFacets(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	ctx := t.Context()
	s, collection, nodes, profile := collectionCoverageFixtureWithStore(t, open(t), 3)
	first, second, pending := nodes[0], nodes[1], nodes[2]
	_, err := s.CreateFile(ctx, s.RootID(), "outside.pdf", first.BlobHash, first.Size, first.MimeType)
	require.NoError(t, err)
	label := "Synthetic review"
	_, err = s.SetCollectionLabel(ctx, collection.ID(), 1, &label)
	require.NoError(t, err)
	tag, err := s.CreateTag(ctx, "Review")
	require.NoError(t, err)
	for _, node := range []Node{first, pending} {
		_, err = s.AssignTag(ctx, tag.ID, node.ID, node.Revision)
		require.NoError(t, err)
	}
	collectionCoveragePublish(t, s, first, profile, "complete")
	collectionCoveragePublish(t, s, second, profile, "complete")

	payload := fmt.Appendf(nil, `{"filters":{"collection_ids":[%q],"tag_ids":[%q],"text_coverage":["complete"]}}`, collection.ID(), tag.ID)
	definition, err := s.CreateSavedQuery(ctx, label, "", SavedQueryKindQuery, payload)
	require.NoError(t, err)
	service := NewQuerySnapshotService(s)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	request := SnapshotRequest{
		Coverage: CoverageSelection{Configuration: "configured", ProfileFingerprint: profile.Fingerprint},
		Facets:   []string{"collections", "tags", "text_coverage", "duplicates"},
	}
	initial, page, err := service.RunSaved(ctx, "owner", definition.ID, definition.Revision, request, nil)
	require.NoError(t, err)
	require.Len(t, page.Rows, 1)
	require.Equal(t, first.ID, page.Rows[0].NodeID)
	require.Equal(t, page.MemberHash, initial.MemberHash)
	require.Len(t, page.Facets, 4)
	require.Equal(t, []SnapshotFacetValue{{Key: collection.ID(), Label: label, Count: 1, Selected: true}}, page.Facets[0].Values)
	require.EqualValues(t, 2, *page.Facets[1].Total)
	require.EqualValues(t, 1, *page.Facets[1].Missing)
	require.EqualValues(t, 2, *page.Facets[2].Total)
	require.EqualValues(t, 1, facetCounts(page.Facets[2])["unprocessed"])
	require.EqualValues(t, 1, facetCounts(page.Facets[3])["duplicate"])

	_, err = s.AssignTag(ctx, tag.ID, second.ID, second.Revision)
	require.NoError(t, err)
	next, updated, err := service.RunSaved(ctx, "owner", definition.ID, definition.Revision, request, nil)
	require.NoError(t, err)
	require.Len(t, updated.Rows, 2)
	require.Equal(t, initial.RunID, next.PreviousRunID)
	require.EqualValues(t, 1, next.Comparison.TotalDelta)
	require.Equal(t, next, persistedSavedQueryRun(t, s, next.RunID))

	emptyTag, err := s.CreateTag(ctx, "No members")
	require.NoError(t, err)
	empty := snapshotTestQuery(t, fmt.Sprintf(`{"filters":{"collection_ids":[%q],"tag_ids":[%q]}}`, collection.ID(), emptyTag.ID))
	zero, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: empty, Facets: []string{"collections", "tags"}})
	require.NoError(t, err)
	require.Empty(t, zero.Rows)
	require.Len(t, zero.Facets, 2)
	require.Equal(t, []SnapshotFacetValue{{Key: collection.ID(), Label: label, Selected: true}}, zero.Facets[0].Values)
	require.Contains(t, zero.Facets[1].Values, SnapshotFacetValue{Key: emptyTag.ID, Label: emptyTag.Name, Selected: true})
}

func checkDocumentSnapshotLexicalTerms(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	s := open(t)
	ctx := t.Context()
	for _, doc := range []struct{ name, text string }{
		{"harbor-record.txt", "ordinary witness notes"},
		{"close.txt", "cobalt harbor map"},
		{"apart.txt", "cobalt wide harbor map"},
	} {
		node, err := s.CreateFile(ctx, s.RootID(), doc.name, testSHA256([]byte(doc.name)), 50, "text/plain")
		require.NoError(t, err)
		require.NoError(t, s.RecordExtraction(ctx, ExtractionResult{
			BlobHash: node.BlobHash, Extractor: "synthetic", ExtractorVersion: 1, Status: ExtractionOK, Text: doc.text,
		}))
	}
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{`{"text":"coba har"}`, []string{"apart.txt", "close.txt"}},
		{`{"syntax":"advanced","text":"\"cobalt harbor\""}`, []string{"close.txt"}},
		{`{"syntax":"advanced","text":"name:harb*"}`, []string{"harbor-record.txt"}},
		{`{"syntax":"advanced","text":"name:harb* OR \"cobalt harbor\""}`, []string{"close.txt", "harbor-record.txt"}},
		{`{"syntax":"advanced","text":"cobalt AND NOT \"cobalt harbor\""}`, []string{"apart.txt"}},
	} {
		t.Run(tc.query, func(t *testing.T) {
			projection, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: snapshotTestQuery(t, tc.query)})
			require.NoError(t, err)
			names := make([]string, 0, len(projection.Rows))
			for _, row := range projection.Rows {
				names = append(names, row.Name)
			}
			require.Equal(t, tc.want, names)
		})
	}
}

func checkDocumentSnapshotLexicalNULPostgres(t *testing.T) {
	t.Helper()
	s := newPostgresQueryStore(t)
	ctx := t.Context()
	for _, doc := range []struct{ name, text string }{
		{"adjacent.txt", "cobalt harbor"},
		{"separate.txt", "cobalt wide harbor"},
	} {
		node, err := s.CreateFile(ctx, s.RootID(), doc.name, testSHA256([]byte(doc.name)), int64(len(doc.text)), "text/plain")
		require.NoError(t, err)
		require.NoError(t, s.RecordExtraction(ctx, ExtractionResult{
			BlobHash: node.BlobHash, Extractor: "synthetic", ExtractorVersion: 1, Status: ExtractionOK, Text: doc.text,
		}))
	}
	const raw = `{"syntax":"advanced","text":"\"cobalt\u0000harbor\""}`
	value := snapshotTestQuery(t, raw)
	canonical, fingerprint, err := query.CanonicalWithFingerprint(value)
	require.NoError(t, err)
	require.Contains(t, string(canonical), `cobalt\u0000harbor`)

	projection, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value})
	require.NoError(t, err)
	require.Len(t, projection.Rows, 1)
	require.Equal(t, "adjacent.txt", projection.Rows[0].Name)
	require.Equal(t, "\"cobalt\x00harbor\"", projection.Query.Text)
	require.Equal(t, fingerprint, projection.QueryFingerprint)

	definition, err := s.CreateSavedQuery(ctx, "Harbor phrase", "", SavedQueryKindQuery, []byte(raw))
	require.NoError(t, err)
	service := NewQuerySnapshotService(s)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	run, page, err := service.RunSaved(ctx, "owner", definition.ID, definition.Revision, SnapshotRequest{}, nil)
	require.NoError(t, err)
	require.Len(t, page.Rows, 1)
	require.Equal(t, "adjacent.txt", page.Rows[0].Name)
	require.Equal(t, value.Text, page.Query.Text)
	require.Equal(t, fingerprint, run.QueryFingerprint)
	record, err := s.SavedQueryByID(ctx, definition.ID)
	require.NoError(t, err)
	require.Equal(t, canonical, record.Payload)
	require.Equal(t, fingerprint, record.Fingerprint)
}

func checkMediaFamilyQueries(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	s := open(t)
	ctx := t.Context()
	newFile := func(name, mimeType string) Node {
		t.Helper()
		node, err := s.CreateFile(ctx, s.RootID(), name, testSHA256([]byte(name)), 12, mimeType)
		require.NoError(t, err)
		return node
	}
	image := newFile("camera.PDF", `IMAGE/PNG ; name="a;b"`)
	fallback := newFile("Résumé.PDF", `application/octet-stream; name="copy"`)
	plain := newFile("notes.TXT", "")
	broken := newFile("broken.PDF", `application/pdf; name="unterminated`)
	dotfile := newFile(".pdf", "")
	for _, tc := range []struct {
		raw  string
		want []int64
	}{
		{`{"syntax":"advanced","text":"media_family:document"}`, []int64{fallback.ID}},
		{`{"filters":{"media_families":["document","text"]}}`, []int64{fallback.ID, plain.ID}},
		{`{"filters":{"media_families":["image"]}}`, []int64{image.ID}},
		{`{"syntax":"advanced","text":"media_family:unknown"}`, []int64{broken.ID, dotfile.ID}},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			result, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: snapshotTestQuery(t, tc.raw)})
			require.NoError(t, err)
			ids := make([]int64, 0, len(result.Rows))
			for _, row := range result.Rows {
				ids = append(ids, row.NodeID)
			}
			require.ElementsMatch(t, tc.want, ids)
		})
	}
	facets, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{
		Query:  snapshotTestQuery(t, `{"filters":{"extensions":["pdf"],"media_families":["document"]}}`),
		Facets: []string{"media_family"},
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, facets.Total)
	family := facetByDimension(t, facets, "media_family")
	require.EqualValues(t, 3, *family.Total)
	counts := facetCounts(family)
	require.EqualValues(t, 1, counts["document"])
	require.EqualValues(t, 1, counts["image"])
	require.EqualValues(t, 1, counts["unknown"])

	saved, err := s.CreateSavedQuery(ctx, "Documents only", "", SavedQueryKindQuery,
		[]byte(`{"filters":{"media_families":["document"]}}`))
	require.NoError(t, err)
	service := NewQuerySnapshotService(s)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	nested := snapshotTestQuery(t, `{"syntax":"advanced","text":"saved:\"Documents only\""}`)
	before, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: nested})
	require.NoError(t, err)
	require.Len(t, before.Rows, 1)
	require.Equal(t, fallback.ID, before.Rows[0].NodeID)
	initial, _, err := service.RunSaved(ctx, "owner", saved.ID, saved.Revision, SnapshotRequest{}, nil)
	require.NoError(t, err)

	_, _, err = s.Move(ctx, fallback.ID, s.RootID(), "Résumé.JPG", fallback.Revision)
	require.NoError(t, err)
	replaced, _, err := s.ReplaceContent(ctx, image.ID, image.Revision, testSHA256([]byte("replacement")), 13, "application/octet-stream")
	require.NoError(t, err)
	next, current, err := service.RunSaved(ctx, "owner", saved.ID, saved.Revision, SnapshotRequest{}, nil)
	require.NoError(t, err)
	require.Equal(t, initial.RunID, next.PreviousRunID)
	require.Len(t, current.Rows, 1)
	require.Equal(t, replaced.CurrentVersionID, current.Rows[0].ContentVersionID)
	require.Equal(t, "document", current.Rows[0].MediaFamily)
	_, reverted, _, err := s.RevertContent(ctx, replaced.ID, replaced.Revision, image.CurrentVersionID)
	require.NoError(t, err)
	require.Equal(t, image.MimeType, reverted.MimeType)
	restored, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: nested})
	require.NoError(t, err)
	require.Empty(t, restored.Rows)
}

func checkMediaFamilyRestorePostgres(t *testing.T) {
	t.Helper()
	ctx := t.Context()
	source := newTestStore(t)
	image, err := source.CreateFile(ctx, source.RootID(), "image.pdf", testSHA256([]byte("image")), 10, "image/png")
	require.NoError(t, err)
	const malformed = "application/pdf; name=\"copy\x00title\""
	file, err := source.CreateFile(ctx, source.RootID(), "invalid.pdf", testSHA256([]byte("invalid")), 10, malformed)
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(ctx, &exported))
	setup, owner := pgtest.NewDatabase(t)
	cfg := pgschema.Config{Schema: "documents", SchemaOwner: owner}
	release := pgtest.ReserveSchemaInstall(t)
	err = ImportPostgresMetadata(ctx, setup, cfg, bytes.NewReader(exported.Bytes()))
	release()
	require.NoError(t, err)
	restored := newPostgresSchemaStore(t, setup, cfg)

	for _, tc := range []struct {
		family string
		id     int64
	}{{"image", image.ID}, {"unknown", file.ID}} {
		value := compilerQuery(t, "media_family:"+tc.family)
		result, err := restored.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: value})
		require.NoError(t, err)
		require.Len(t, result.Rows, 1)
		require.Equal(t, tc.id, result.Rows[0].NodeID)
	}
	version, err := restored.ContentVersionByID(ctx, file.CurrentVersionID)
	require.NoError(t, err)
	require.Equal(t, malformed, version.MimeType)
	var reexported bytes.Buffer
	require.NoError(t, restored.ExportMetadata(ctx, &reexported))
	require.Equal(t, exported.Bytes(), reexported.Bytes())
}
