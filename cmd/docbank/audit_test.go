package main

import (
	"bytes"
	"io"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
)

func TestHumanAuditOutputQuotesPaths(t *testing.T) {
	const unsafePath = "/Taxes/\n\x1b[31mFORGED"
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
				return writeAuditEnabled(w, api.AuditStatus{Scopes: []api.AuditScopeStatus{{
					TargetPath: unsafePath, TargetNodeID: 42,
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
		for _, class := range []string{"names", "topology", "tags", "assignments", "ingests", "provenance"} {
			assert.Contains(t, text, class)
		}
	}
}
