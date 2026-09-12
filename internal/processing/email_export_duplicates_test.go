package processing

import (
	"bytes"
	"encoding/json/v2"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/store"
)

func TestExportExplicitCollapsePreservesAllOccurrenceReceipts(t *testing.T) {
	f := newEmailPipelineFixture(t)
	var nodes []int64
	for _, name := range []string{"first.eml", "second.eml"} {
		target := f.add(t, name, emailPipelineSource, "message/rfc822")
		view, err := EnsureEmailTarget(t.Context(), f.catalog, f.blobs, f.spool, target)
		require.NoError(t, err)
		_, err = PublishEmailDocuments(t.Context(), f.catalog, f.blobs, pipelineDocumentRequest(t, f, view, name))
		require.NoError(t, err)
		nodes = append(nodes, target.Version.NodeID)
	}
	source, err := f.catalog.CreateExportSource(t.Context(), "owner", bundle.SourceRequest{OperationID: uuid.NewString(), Kind: "nodes", NodeIDs: nodes}, nil)
	require.NoError(t, err)
	r := bundle.PlanRequest{OperationID: uuid.NewString(), SourceID: source.ID, MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "original"}, {Role: "attachment_original"}}}
	preserved, err := f.catalog.CreateExportPlan(t.Context(), "owner", r)
	require.NoError(t, err)
	require.Equal(t, 6, preserved.RoleEntries)
	r.OperationID = uuid.NewString()
	raw, err := json.Marshal(r)
	require.NoError(t, err)
	raw = append(raw[:len(raw)-1], []byte(",\"duplicate_policy\":\"collapse_exact_content\"}")...)
	require.NoError(t, json.Unmarshal(raw, &r))
	plan, err := f.catalog.CreateExportPlan(t.Context(), "owner", r)
	require.NoError(t, err)
	require.Equal(t, 2, plan.Total)
	require.Equal(t, 6, plan.Rows())
	require.Equal(t, 3, plan.RoleEntries)
	encoded, err := json.Marshal(plan)
	require.NoError(t, err)
	var summary struct {
		Counts struct {
			Messages    int `json:"messages"`
			Attachments int `json:"attachments"`
			Collapsed   int `json:"collapsed"`
		} `json:"counts"`
	}
	require.NoError(t, json.Unmarshal(encoded, &summary))
	require.Equal(t, 2, summary.Counts.Messages)
	require.Equal(t, 4, summary.Counts.Attachments)
	require.Equal(t, 3, summary.Counts.Collapsed)
	walk := func(visit func(bundle.Document) error) error {
		return f.catalog.WalkExportDocuments(t.Context(), plan.ID, visit)
	}
	var parents, children, collapsed int
	require.NoError(t, walk(func(d bundle.Document) error {
		if d.Attachment == nil {
			parents++
		} else {
			children++
		}
		for _, role := range d.Roles {
			if role.Status == "collapsed" {
				collapsed++
			}
		}
		return nil
	}))
	require.Equal(t, 2, parents)
	require.Equal(t, 4, children)
	require.Equal(t, 3, collapsed)
	file, err := os.Create(filepath.Join(t.TempDir(), "collapsed.zip"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, file.Close()) })
	receipt, err := bundle.Write(t.Context(), file, plan, walk, func(r bundle.Role) (io.ReadCloser, error) {
		stream, _, e := f.blobs.OpenStreamContext(t.Context(), r.SHA256)
		return stream, e
	}, nil)
	require.NoError(t, err)
	_, err = bundle.Verify(t.Context(), file, receipt.Size, plan.Fingerprint)
	require.NoError(t, err)
	for _, change := range []string{"foreign reference", "missing reference", "changed count"} {
		t.Run(change, func(t *testing.T) {
			p := plan
			counts := *plan.Counts
			p.Counts = &counts
			if change == "changed count" {
				p.Counts.Collapsed++
			}
			validator := bundle.RowValidator{Plan: p}
			failure := walk(func(d bundle.Document) error {
				d.Roles = slices.Clone(d.Roles)
				for i := range d.Roles {
					if d.Roles[i].Status == "collapsed" {
						switch change {
						case "foreign reference":
							d.Roles[i].ReuseOf = "documents/foreign/original"
						case "missing reference":
							d.Roles[i].ReuseOf = ""
						}
					}
				}
				return validator.Add(d)
			})
			if failure == nil {
				failure = validator.Finish()
			}
			require.ErrorIs(t, failure, bundle.ErrConflict)
		})
	}
	var metadata bytes.Buffer
	require.NoError(t, f.catalog.ExportMetadata(t.Context(), &metadata))
	restored, err := store.Open(filepath.Join(t.TempDir(), "collapsed-restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(metadata.Bytes())))
	require.NoError(t, restored.ValidateMetadata(t.Context()))
}
