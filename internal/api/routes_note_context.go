package api

import (
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"path"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/document/notemarkdown"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

const noteContextMaxBytes = 60 << 10
const noteProjectionReady = "ready"

type noteCoverageParameter struct {
	Value NoteCoverageSelection
	Set   bool
}

func (p *noteCoverageParameter) Schema(r huma.Registry) *huma.Schema {
	return r.Schema(reflect.TypeFor[NoteCoverageSelection](), true, "")
}
func (p *noteCoverageParameter) Receiver() reflect.Value    { return reflect.ValueOf(&p.Value).Elem() }
func (p *noteCoverageParameter) OnParamSet(set bool, _ any) { p.Set = set }

func registerNoteContextRoute(api huma.API, d Deps, service *documentQueryService) {
	type input struct {
		Publication string                `query:"publication" required:"true" maxLength:"64"`
		SourceRef   string                `query:"source_ref" required:"true" maxLength:"4096"`
		Version     string                `query:"content_version_id" maxLength:"36"`
		Direction   string                `query:"direction" enum:"outgoing,incoming,unresolved" default:"outgoing"`
		Limit       int                   `query:"limit" minimum:"1" maximum:"50" default:"50"`
		Cursor      string                `query:"cursor" maxLength:"32768"`
		Coverage    noteCoverageParameter `query:"coverage,deepObject"`
	}
	huma.Register(api, huma.Operation{OperationID: "getNoteContext", Method: http.MethodGet,
		Path: "/api/v1/notes/context", Summary: "Read current authored-note context"},
		func(ctx context.Context, in *input) (*struct{ Body NoteContextPage }, error) {
			request := NoteContextRequest{Publication: in.Publication, SourceRef: in.SourceRef, ContentVersionID: in.Version,
				Direction: in.Direction, Limit: in.Limit, Cursor: in.Cursor}
			if in.Coverage.Set {
				if in.Coverage.Value.Profile == "" {
					return nil, NewError(422, "validation", "coverage profile is required")
				}
				request.Coverage = &in.Coverage.Value
			}
			page, err := service.noteContext(ctx, d, request)
			if err != nil {
				return nil, err
			}
			return &struct{ Body NoteContextPage }{page}, nil
		})
}

func (service *documentQueryService) noteContext(ctx context.Context, d Deps, request NoteContextRequest) (NoteContextPage, error) {
	query := store.NoteContextRequest{Publication: request.Publication, SourceRef: request.SourceRef,
		ContentVersionID: request.ContentVersionID, Direction: request.Direction, Limit: request.Limit}
	if request.Cursor != "" {
		version, position, err := service.decodeNoteContextCursor(request)
		if err != nil {
			return NoteContextPage{}, noteContextError(err)
		}
		query.ContentVersionID, query.After = version, position
	}
	page, err := service.store.NoteContext(ctx, query)
	if err != nil {
		return NoteContextPage{}, noteContextError(err)
	}
	wire, err := noteContextWire(page)
	if err != nil {
		return NoteContextPage{}, noteContextError(err)
	}
	if request.Coverage != nil {
		if d.Processing == nil {
			return NoteContextPage{}, processingUnavailable()
		}
		report, err := d.Processing.Coverage(ctx, request.Coverage.Profile, processing.SourceFence{
			VaultUID: page.Source.Identity.VaultID, ContentVersionIDs: []string{page.Source.Identity.ContentVersionID},
		})
		if err != nil {
			return NoteContextPage{}, fromProcessingError(err)
		}
		if request.Coverage.BindingID != "" {
			var selected *processing.CoverageClass
			for i := range report.Embeddings {
				if report.Embeddings[i].Name == request.Coverage.BindingID {
					selected = &report.Embeddings[i]
					break
				}
			}
			if selected == nil {
				return NoteContextPage{}, NewError(422, "validation", "coverage binding is not configured")
			}
			report.Embeddings = []processing.CoverageClass{*selected}
			report.State = "complete"
			if report.Renditions.Required && report.Renditions.Complete+report.Renditions.Rebuilding != report.Renditions.Total || selected.Complete+selected.Rebuilding != selected.Total {
				report.State = "partial"
			} else if report.Renditions.Rebuilding > 0 || selected.Rebuilding > 0 {
				report.State = "rebuilding"
			}
		}
		coverage := fromProcessingCoverage(report)
		wire.Readiness.Coverage, wire.Readiness.CoverageSelection = &coverage, request.Coverage
		wire.Readiness.CoverageObservedAt = service.now().UTC().Format(time.RFC3339Nano)
	}
	for {
		wire.NextCursor = ""
		if len(wire.Items) > 0 && (len(wire.Items) < len(page.Edges) || page.Next != nil) {
			last := page.Edges[len(wire.Items)-1]
			position := store.NoteContextPosition{GenerationID: page.Generation.ID, DatabaseEpoch: page.Status.DatabaseEpoch,
				NamespaceRevision: page.Status.NamespaceRevision, SourceRef: last.Source.SourceRef, Ordinal: last.Edge.Ordinal}
			wire.NextCursor, err = service.encodeNoteContextCursor(request, page.Source.Identity.ContentVersionID, position)
			if err != nil {
				return NoteContextPage{}, noteContextError(err)
			}
		}
		encoded, err := json.Marshal(wire)
		if err != nil {
			return NoteContextPage{}, noteContextError(err)
		}
		if len(encoded) < noteContextMaxBytes {
			return wire, nil
		}
		if len(wire.Items) <= 1 {
			return NoteContextPage{}, NewError(422, "note_context_too_large", "one note context item exceeds the response limit")
		}
		wire.Items = wire.Items[:len(wire.Items)-1]
	}
}

func noteContextWire(page store.NoteContextPage) (NoteContextPage, error) {
	wire := NoteContextPage{Source: PublishedNoteIdentity(page.Source.Identity), Metadata: NoteMetadata{Aliases: []string{}, Tags: []string{}},
		Diagnostics: []NoteDiagnostic{}, Items: make([]NoteEdge, 0, len(page.Edges)), TruncatedFields: []string{},
		Readiness: NoteReadiness{Publisher: fromNotePublicationStatus(page.Status),
			Parse: NoteProjectionReadiness{State: "pending", Recipe: notemarkdown.Recipe},
			Graph: NoteProjectionReadiness{State: "pending", Recipe: notemarkdown.ResolverRecipe}}}
	if page.Source.ArchiveHead != nil {
		wire.ArchiveHead = new(NoteArchiveHead(*page.Source.ArchiveHead))
	}
	var note notemarkdown.Note
	if page.ParseFound {
		wire.Readiness.Parse.State, wire.Readiness.Parse.Reason = page.Parse.State, page.Parse.Reason
		if page.Parse.State == noteProjectionReady {
			if err := json.Unmarshal(page.Parse.Artifact, &note, json.RejectUnknownMembers(true)); err != nil {
				return NoteContextPage{}, err
			}
		}
	}
	if page.GraphReady {
		wire.Readiness.Graph.State, wire.Readiness.Graph.Generation = noteProjectionReady, page.Generation.ID
	}
	clip := func(value string, limit int, field string) string {
		if len(value) <= limit {
			return value
		}
		for limit > 0 && !utf8.ValidString(value[:limit]) {
			limit--
		}
		if !slices.Contains(wire.TruncatedFields, field) {
			wire.TruncatedFields = append(wire.TruncatedFields, field)
		}
		return value[:limit]
	}
	wire.Metadata.Title = clip(note.Metadata.Title, 512, "metadata.title")
	wire.DisplayTitle = wire.Metadata.Title
	if wire.DisplayTitle == "" {
		base := path.Base(page.Source.Identity.SourceRef)
		title := strings.TrimSuffix(base, path.Ext(base))
		if title == "" {
			title = base
		}
		wire.DisplayTitle = clip(title, 512, "display_title")
	}
	for i, alias := range note.Metadata.Aliases {
		if i >= 16 {
			wire.Metadata.AliasesOmitted++
			continue
		}
		wire.Metadata.Aliases = append(wire.Metadata.Aliases, clip(alias, 256, "metadata.aliases"))
	}
	for i, tag := range note.Metadata.Tags {
		if i >= 16 {
			wire.Metadata.TagsOmitted++
			continue
		}
		wire.Metadata.Tags = append(wire.Metadata.Tags, clip(tag, 256, "metadata.tags"))
	}
	for _, diagnostic := range note.Diagnostics {
		wire.Diagnostics = append(wire.Diagnostics, NoteDiagnostic{Code: diagnostic.Code, Span: NoteSpan(diagnostic.Span)})
	}
	for _, item := range page.Edges {
		hash := sha256.Sum256([]byte(item.Source.SourceRef))
		edge := NoteEdge{Key: fmt.Sprintf("%x:%d", hash, item.Edge.Ordinal), Form: item.Edge.Link.Form, State: item.Edge.State,
			RawTarget: item.Edge.Link.RawTarget, Target: item.Edge.Link.Target, Embed: item.Edge.Link.Embed,
			Source: PublishedNoteIdentity(item.Source), Span: NoteSpan(item.Edge.Link.Span), DestinationSpan: NoteSpan(item.Edge.Link.DestinationSpan)}
		if item.Edge.Target != nil {
			edge.TargetIdentity = new(PublishedNoteIdentity(*item.Edge.Target))
		}
		if item.Edge.TargetHeading != nil {
			edge.TargetHeading = new(NoteSpan(*item.Edge.TargetHeading))
		}
		wire.Items = append(wire.Items, edge)
	}
	return wire, nil
}

func noteContextError(err error) error {
	switch {
	case errors.Is(err, store.ErrNoteContextStale):
		return NewError(409, "note_context_stale", "note context changed; start a new page")
	case errors.Is(err, store.ErrNoteGraphUnavailable):
		return NewError(409, "note_graph_unavailable", "note graph is unavailable for that version")
	case errors.Is(err, store.ErrInvalidNoteProjection):
		return NewError(422, "validation", "invalid note context request")
	default:
		return notePublicationError(err)
	}
}
