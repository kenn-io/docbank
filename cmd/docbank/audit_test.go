package main

import (
	"bytes"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestHumanAuditHistoryShowsPhotoDecisions(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	node, err := s.CreateFile(t.Context(), s.RootID(), "photo.jpg", strings.Repeat("a", 64), 4, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	fileID := asset.Files[0].ID
	plan, err := s.PreviewInitialAudit(t.Context(), s.RootID(), "cli", nil)
	require.NoError(t, err)
	_, err = s.EnableInitialAudit(t.Context(), plan)
	require.NoError(t, err)
	_, err = s.EditPhotoAuthored(t.Context(), []store.PhotoAuthoredTarget{{FileID: fileID, Revision: 1, Patch: store.PhotoAuthoredPatch{Rating: new(5), Flag: new("pick"), Label: new("red"), Caption: new("River\n\x1b[31m"), Creator: new("Example photographer"), Copyright: new("Example rights"), Rotation: new(90)}}})
	require.NoError(t, err)
	page, err := s.AuditHistory(t.Context(), node.ID, 10, "")
	require.NoError(t, err)
	change := page.Items[0].Attachment
	require.NotNil(t, change)
	require.Equal(t, node.ID, change.Identity.NodeID)
	events := []api.AuditEvent{{NodeID: node.ID, Kind: "photo_authored", Attachment: &api.AuditAttachmentChange{
		Kind: change.Kind, Identity: api.AuditAttachmentIdentity{FileID: change.Identity.FileID, NodeID: change.Identity.NodeID},
		Before: &api.AuditAttachmentState{Photo: change.Before.Photo}, After: &api.AuditAttachmentState{Photo: change.After.Photo},
	}}}
	for _, scope := range []bool{false, true} {
		t.Run(strconv.FormatBool(scope), func(t *testing.T) {
			var output bytes.Buffer
			if scope {
				require.NoError(t, writeAuditScopeHistory(&output, api.AuditScopeEventPage{Items: events, Total: 1}))
			} else {
				require.NoError(t, writeAuditHistory(&output, api.AuditEventPage{Node: api.Node{ID: node.ID}, Items: events, Total: 1}))
			}
			for _, want := range []string{fileID, "on id:" + strconv.FormatInt(node.ID, 10), "revision 1", "revision 2", "rating 0", "rating 5", `flag "pick"`, `label "red"`, `caption "River\n\x1b[31m"`, `creator "Example photographer"`, `copyright "Example rights"`, "rotation 90"} {
				assert.Contains(t, output.String(), want)
			}
			assert.NotContains(t, output.String(), "\x1b")
		})
	}
}

func TestHumanAuditOutputQuotesPaths(t *testing.T) {
	const unsafePath = "/Taxes/\n\x1b[31mFORGED"
	const scopeID = "11111111-1111-4111-8111-111111111111"
	want := strconv.QuoteToASCII(unsafePath)
	tests := []struct {
		name  string
		write func(io.Writer) error
	}{
		{
			name: "preview",
			write: func(w io.Writer) error {
				return writeAuditPreview(w, api.AuditEnrollmentPreview{
					TargetPath: unsafePath, TargetNodeID: 42,
				})
			},
		},
		{
			name: "enabled",
			write: func(w io.Writer) error {
				return writeAuditEnabled(w, api.AuditStatus{EnabledScopeID: scopeID, Scopes: []api.AuditScopeStatus{{
					ID: scopeID, TargetPath: unsafePath, TargetNodeID: 42,
				}}})
			},
		},
		{
			name: "status",
			write: func(w io.Writer) error {
				return writeAuditStatus(w, api.AuditStatus{
					Enabled: true,
					Scopes: []api.AuditScopeStatus{{
						TargetPath: unsafePath, TargetNodeID: 42,
					}},
					Membership: &api.AuditMembershipStatus{
						NodeID: 42, Path: unsafePath,
					},
				})
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			require.NoError(t, test.write(&output))
			assert.Contains(t, output.String(), want)
			assert.Contains(t, output.String(), "id:42")
			assert.NotContains(t, output.String(), unsafePath)
		})
	}
}

func TestHumanAuditHistoryUsesCopyableNodeSelectors(t *testing.T) {
	var output bytes.Buffer
	require.NoError(t, writeAuditHistory(&output, api.AuditEventPage{
		Node: api.Node{ID: 42, TrashedAt: "2026-07-20T00:00:00Z"},
		Items: []api.AuditEvent{{
			Kind: "tag_assign",
			Attachment: &api.AuditAttachmentChange{
				Kind: "tag_assignment",
				Identity: api.AuditAttachmentIdentity{
					TagID: "33333333-3333-4333-8333-333333333333", NodeID: 42,
				},
			},
		}},
		Total: 1,
	}))
	assert.Contains(t, output.String(), "audit history for id:42 in trash (id:42)")
	assert.Contains(t, output.String(), "on id:42")
	assert.NotContains(t, output.String(), "node 42")
}

func TestHumanAuditScopeHistoryQuotesTargetAndNamesEventNodes(t *testing.T) {
	var output bytes.Buffer
	require.NoError(t, writeAuditScopeHistory(&output, api.AuditScopeEventPage{
		Scope: api.AuditScopeStatus{
			ID:           "33333333-3333-4333-8333-333333333333",
			TargetNodeID: 7, TargetPath: "/Taxes/\n\x1b[31mFORGED",
		},
		Items: []api.AuditEvent{{NodeID: 42, Kind: "content_replace"}},
		Total: 1,
	}))
	assert.Contains(t, output.String(), strconv.QuoteToASCII("/Taxes/\n\x1b[31mFORGED"))
	assert.Contains(t, output.String(), "id:42")
	assert.NotContains(t, output.String(), "/Taxes/\n\x1b[31mFORGED")
}

func TestAuditRetentionDisclosureNamesEveryMetadataClass(t *testing.T) {
	var output bytes.Buffer
	require.NoError(t, writeAuditPreview(&output, api.AuditEnrollmentPreview{}))
	help := auditEnableCmd.Flags().Lookup("acknowledge-permanent-retention").Usage
	for _, text := range []string{output.String(), help} {
		for _, class := range []string{"names", "topology", "tags", "assignments", "ingests", "provenance", "photo decisions", "captions", "creators", "copyrights"} {
			assert.Contains(t, text, class)
		}
	}
}
