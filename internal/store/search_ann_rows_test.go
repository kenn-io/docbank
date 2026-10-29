package store

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/vectorindex"
)

func TestSemanticANNRowsMatchEligibleMemberships(t *testing.T) {
	t.Parallel()
	for _, kind := range []document.EmbeddingInputKind{document.EmbeddingInputOriginalFile, document.EmbeddingInputRenditionChunk} {
		t.Run(string(kind), func(t *testing.T) {
			s, version, profile, attachment := newEmbeddingCatalogFixture(t)
			binding := "optional"
			if kind == document.EmbeddingInputRenditionChunk {
				binding = "chunk"
			}
			if kind == document.EmbeddingInputOriginalFile {
				attachment = ""
			}
			record := embeddingSetFixture(s, version, profile.Fingerprint, kind, binding, attachment)
			publish := func(record EmbeddingSetRecord) {
				require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
				require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{FencingToken: 1, Key: EmbeddingHeadKey{ContentVersionID: record.ContentVersionID, BindingID: binding, InputKind: kind}, SetID: record.ID, VectorSpaceID: record.VectorSpace.ID, ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime}))
			}
			publish(record)
			if kind == document.EmbeddingInputRenditionChunk {
				var build string
				require.NoError(t, s.db.QueryRow("SELECT build_id FROM rendition_attachments WHERE attachment_id=?", attachment).Scan(&build))
				duplicate, err := s.CreateFile(t.Context(), s.RootID(), "duplicate-scoring-rows.pdf", catalogSourceHash, 20, "application/pdf")
				require.NoError(t, err)
				copyAttachment := RenditionAttachmentRecord{ID: testSHA256([]byte("duplicate-ann-attachment")), VaultID: s.VaultID(), ContentVersionID: duplicate.CurrentVersionID, BuildID: build, Profile: profile, AttachedAt: embeddingCatalogTime}
				require.NoError(t, publishAttachmentForTest(t, s, copyAttachment))
				copyRecord := embeddingSetFixture(s, duplicate.CurrentVersionID, profile.Fingerprint, kind, binding, copyAttachment.ID)
				require.Equal(t, record.VectorSet.ID, copyRecord.VectorSet.ID)
				publish(copyRecord)
			}
			content, err := s.ContentVersionByID(t.Context(), version)
			require.NoError(t, err)
			node, err := s.NodeByID(t.Context(), content.NodeID)
			require.NoError(t, err)
			directory, err := s.Mkdir(t.Context(), s.RootID(), "selected-ann")
			require.NoError(t, err)
			node, _, err = s.Move(t.Context(), node.ID, directory.ID, node.Name, node.Revision)
			require.NoError(t, err)
			tag, err := s.CreateTag(t.Context(), "selected-ann")
			require.NoError(t, err)
			_, err = s.AssignTag(t.Context(), tag.ID, node.ID, node.Revision)
			require.NoError(t, err)
			for _, tc := range []struct {
				name    string
				scope   SearchOptions
				matches bool
			}{
				{"all", SearchOptions{}, true},
				{"explicit", SearchOptions{ContentVersionIDs: []string{version}}, true},
				{"unknown version", SearchOptions{ContentVersionIDs: []string{"11111111-1111-4111-8111-111111111111"}}, false},
				{"mime", SearchOptions{MIMEType: "application/pdf"}, true},
				{"other mime", SearchOptions{MIMEType: "text/plain"}, false},
				{"tag", SearchOptions{TagID: tag.ID}, true},
				{"directory", SearchOptions{UnderNodeID: directory.ID}, true},
				{"modified range", SearchOptions{ModifiedSince: "2020-01-01T00:00:00Z", ModifiedBefore: "2099-01-01T00:00:00Z"}, true},
				{"old range", SearchOptions{ModifiedBefore: "2000-01-01T00:00:00Z"}, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					normalized, err := s.NormalizeSearchOptions(t.Context(), tc.scope)
					require.NoError(t, err)
					filterSQL, args := searchFilterSQL(normalized)
					require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
						rows, err := loadSemanticANNRows(t.Context(), tx, profile.Fingerprint, binding, kind, record.VectorSpace.ID, filterSQL, args)
						if err != nil {
							return err
						}
						members, err := loadSemanticEligibility(t.Context(), tx, profile.Fingerprint, binding, kind, record.VectorSpace.ID, filterSQL, args)
						if err != nil {
							return err
						}
						oldRows := make([]vectorindex.RowIdentity, 0, len(members))
						for key := range members {
							oldRows = append(oldRows, vectorindex.RowIdentity{SetID: key.VectorSetID, InputKey: key.InputID, InputChecksum: key.InputChecksum})
						}
						require.ElementsMatch(t, oldRows, rows)
						var want []vectorindex.RowIdentity
						if tc.matches {
							for _, input := range record.InputGeneration.Inputs {
								want = append(want, vectorindex.RowIdentity{SetID: record.VectorSet.ID, InputKey: input.ID, InputChecksum: input.RenderedChecksum})
							}
						}
						require.ElementsMatch(t, want, rows, "shared chunk rows must be scored once, with exact metadata constraints")
						return nil
					}))
				})
			}
		})
	}
}
