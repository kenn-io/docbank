package processing

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/json/v2"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/store"
)

func TestExportExplicitCollapsePreservesAllOccurrenceReceipts(t *testing.T) {
	t.Parallel()
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
	for _, packaging := range []string{"flat", "volumes"} {
		t.Run(packaging, func(t *testing.T) {
			p := plan
			if packaging == "volumes" {
				r.OperationID = uuid.NewString()
				r.VolumeLimits = &bundle.VolumeLimits{Roles: 1, RoleBytes: 1 << 20}
				p, err = f.catalog.CreateExportPlan(t.Context(), "owner", r)
				require.NoError(t, err)
			}
			file, err := os.Create(filepath.Join(t.TempDir(), "collapsed.zip"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, file.Close()) })
			receipt, err := bundle.Write(t.Context(), file, p, func(visit func(bundle.Document) error) error {
				return f.catalog.WalkExportDocuments(t.Context(), p.ID, visit)
			}, func(r bundle.Role) (io.ReadCloser, error) {
				stream, _, e := f.blobs.OpenStreamContext(t.Context(), r.SHA256)
				return stream, e
			}, nil)
			require.NoError(t, err)
			_, err = bundle.Verify(t.Context(), file, receipt.Size, p.Fingerprint)
			require.NoError(t, err)
			archive, err := zip.NewReader(file, receipt.Size)
			require.NoError(t, err)
			paths := map[string]bool{}
			for _, entry := range archive.File {
				paths[entry.Name] = true
				if strings.HasPrefix(entry.Name, "volumes/") {
					stream, err := entry.Open()
					require.NoError(t, err)
					data, err := io.ReadAll(stream)
					require.NoError(t, err)
					require.NoError(t, stream.Close())
					volume, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
					require.NoError(t, err)
					for _, output := range volume.File {
						paths[output.Name] = true
					}
				}
			}
			metadata, err := archive.Open("metadata.csv")
			require.NoError(t, err)
			rows, err := csv.NewReader(metadata).ReadAll()
			require.NoError(t, err)
			require.NoError(t, metadata.Close())
			require.Len(t, rows, 7)
			for _, row := range rows[1:] {
				require.True(t, paths[row[7]], "CSV role_path must name a physical output: %v", row)
			}
		})
	}
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
