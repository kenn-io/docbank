package api

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/document/notemarkdown"
	"go.kenn.io/docbank/internal/store"
)

// NoteSummary describes the immutable version currently at a live note path.
type NoteSummary struct {
	Path             string   `json:"path"`
	NodeID           int64    `json:"node_id"`
	ContentVersionID string   `json:"content_version_id"`
	SHA256           string   `json:"sha256"`
	Size             int64    `json:"size"`
	ModifiedAt       string   `json:"modified_at"`
	Title            string   `json:"title"`
	Tags             []string `json:"tags"`
	SourceState      string   `json:"source_state" enum:"present,gone,not_watched"`
}

type NoteByPath struct {
	NoteSummary
	Headings         int                       `json:"headings"`
	Frontmatter      map[string]jsontext.Value `json:"frontmatter" nullable:"true"`
	FrontmatterError string                    `json:"frontmatter_error,omitempty" enum:"invalid_frontmatter,unclosed_frontmatter" doc:"Bounded parser diagnostic when frontmatter was discarded; the note body remains readable."`
}

func (n NoteByPath) MarshalJSONTo(enc *jsontext.Encoder) error {
	// Keep absent/discarded YAML null without changing the shared API codec.
	type noteByPath NoteByPath
	return json.MarshalEncode(enc, noteByPath(n), json.FormatNilMapAsNull(true))
}

func (NoteByPath) TransformSchema(_ huma.Registry, schema *huma.Schema) *huma.Schema {
	// Raw members carry arbitrary JSON, rather than base64-encoded bytes.
	schema.Properties["frontmatter"].AdditionalProperties = &huma.Schema{}
	return schema
}

type NoteWatchStatus struct {
	Name        string `json:"name"`
	Destination string `json:"destination"`
	Parsed      int    `json:"parsed"`
	Pending     int    `json:"pending"`
	Failed      int    `json:"failed"`
	LastError   string `json:"last_error,omitempty" doc:"Bounded content failure code for the latest failed current note."`
}

type NotesStatus struct {
	Items []NoteWatchStatus `json:"items"`
}

func registerNoteRoutes(api huma.API, d Deps) {
	type byPathInput struct {
		Path string `query:"path" required:"true"`
	}
	type byPathOutput struct{ Body NoteByPath }
	huma.Register(api, huma.Operation{
		OperationID: "getNoteByPath", Method: http.MethodGet, Path: "/api/v1/notes/by-path",
		Summary: "Read a current Markdown note summary and frontmatter",
	}, func(ctx context.Context, in *byPathInput) (*byPathOutput, error) {
		if !strings.HasPrefix(in.Path, "/") {
			return nil, NewError(http.StatusUnprocessableEntity, "validation",
				fmt.Sprintf("path %q must be absolute (start with /)", in.Path))
		}
		view, err := d.Store.NoteByPath(ctx, in.Path, notemarkdown.Recipe)
		if err != nil {
			return nil, FromStoreError(err)
		}
		if view.State == "pending" {
			return nil, NewError(http.StatusConflict, "note_pending", "Note structure is still being derived")
		}
		if view.State != "ok" {
			message := "Note structure is unavailable for these original bytes"
			if view.State == "not_utf8" {
				message = "Note original contains invalid UTF-8 or NUL bytes"
			}
			return nil, NewError(http.StatusUnprocessableEntity, "note_"+view.State, message)
		}
		n := view.Summary
		return &byPathOutput{Body: NoteByPath{NoteSummary: NoteSummary{
			Path: n.Path, NodeID: n.NodeID, ContentVersionID: n.ContentVersionID, SHA256: n.SHA256, Size: n.Size,
			ModifiedAt: n.ModifiedAt, Title: n.Title, Tags: n.Tags, SourceState: n.SourceState,
		}, Headings: view.HeadingCount, Frontmatter: view.Frontmatter, FrontmatterError: view.FrontmatterError}}, nil
	})
	type statusOutput struct{ Body NotesStatus }
	huma.Register(api, huma.Operation{
		OperationID: "getNotesStatus", Method: http.MethodGet, Path: "/api/v1/notes/status",
		Summary: "Count parsed, pending and failed current notes per watch collection",
	}, func(ctx context.Context, _ *struct{}) (*statusOutput, error) {
		watches := make([]store.NoteWatch, 0, len(d.Cfg.Watches))
		for _, watch := range d.Cfg.Watches {
			watches = append(watches, store.NoteWatch{Name: watch.Name, Destination: watch.Destination})
		}
		items, err := d.Store.NotesStatus(ctx, notemarkdown.Recipe, watches)
		if err != nil {
			return nil, FromStoreError(err)
		}
		out := &statusOutput{Body: NotesStatus{Items: make([]NoteWatchStatus, 0, len(items))}}
		for _, item := range items {
			out.Body.Items = append(out.Body.Items, NoteWatchStatus{
				Name: item.Name, Destination: item.Destination, Parsed: item.Parsed, Pending: item.Pending, Failed: item.Failed, LastError: item.LastError,
			})
		}
		return out, nil
	})
}
