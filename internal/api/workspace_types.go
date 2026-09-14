package api

import (
	"reflect"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/query"
	"go.kenn.io/docbank/internal/store"
)

const maxWorkspaceQueryRequestBytes = query.MaxInputBytes + (32 << 10)

// WorkspaceQueryCreateRequest opens one exact, daemon-lifetime query snapshot.
type WorkspaceQueryCreateRequest struct {
	Query    QueryPayload `json:"query"`
	Profile  string       `json:"profile,omitempty" maxLength:"128"`
	PageSize int          `json:"page_size,omitempty" enum:"50,100,250" default:"100"`
	Facets   []string     `json:"facets,omitempty" maxItems:"8" uniqueItems:"true" enum:"collections,tags,media_family,extension,modified,size,text_coverage,duplicates"`
}

// WorkspaceQueryPageRequest reads one page using only the opaque cursor minted
// for that snapshot. Query and page options cannot change after creation.
type WorkspaceQueryPageRequest struct {
	Cursor string `json:"cursor" minLength:"1" maxLength:"2048"`
}

// SavedQueryRunRequest supplies execution options without allowing callers to
// replace the revision-fenced saved QueryV1 definition.
type SavedQueryRunRequest struct {
	Profile  string   `json:"profile,omitempty" maxLength:"128"`
	PageSize int      `json:"page_size,omitempty" enum:"50,100,250" default:"100"`
	Facets   []string `json:"facets,omitempty" maxItems:"8" uniqueItems:"true" enum:"collections,tags,media_family,extension,modified,size,text_coverage,duplicates"`
}

// WorkspaceQueryDependency is the explicit snake-case wire form of an
// immutable query dependency. query.Dependency intentionally has no JSON tags.
type WorkspaceQueryDependency struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Revision int64  `json:"revision" minimum:"1"`
}

type WorkspaceQueryCoverage struct {
	Configuration      string `json:"configuration" enum:"configured,unconfigured,profile_required"`
	ProfileFingerprint string `json:"profile_fingerprint,omitempty" pattern:"^[0-9a-f]{64}$"`
}

type WorkspaceQueryGeneration struct {
	Kind         string `json:"kind" enum:"native,rendition"`
	GenerationID string `json:"generation_id,omitempty"`
}

type WorkspaceQueryTag struct {
	ID       string `json:"id" format:"uuid"`
	Name     string `json:"name"`
	Revision int64  `json:"revision" minimum:"1"`
}

type WorkspaceQueryRow struct {
	NodeID                 int64               `json:"node_id" minimum:"1"`
	ContentVersionID       string              `json:"content_version_id" format:"uuid"`
	BlobHash               string              `json:"blob_hash" pattern:"^[0-9a-f]{64}$"`
	Size                   int64               `json:"size" minimum:"0"`
	Revision               int64               `json:"revision" minimum:"1"`
	Name                   string              `json:"name"`
	Path                   string              `json:"path"`
	MIMEType               string              `json:"mime_type"`
	MediaFamily            string              `json:"media_family"`
	ModifiedAt             string              `json:"modified_at"`
	SortKey                string              `json:"sort_key"`
	Tags                   []WorkspaceQueryTag `json:"tags"`
	CollectionIDs          []string            `json:"collection_ids"`
	DisplayCollectionID    string              `json:"display_collection_id,omitempty"`
	DisplayCollectionLabel *string             `json:"display_collection_label,omitempty" nullable:"true"`
	CoverageState          string              `json:"coverage_state,omitempty"`
	CoverageBuildID        string              `json:"coverage_build_id,omitempty"`
	CoverageAttachmentID   string              `json:"coverage_attachment_id,omitempty"`
	Excerpt                string              `json:"excerpt,omitempty"`
}

type WorkspaceFacetValue struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Count    int64  `json:"count" minimum:"0"`
	Selected bool   `json:"selected"`
}

// WorkspaceFacet is either available with count pointers or explicitly
// unavailable with nil count pointers and a non-empty reason.
type WorkspaceFacet struct {
	Dimension string                `json:"dimension"`
	Available bool                  `json:"available"`
	Reason    string                `json:"reason,omitempty"`
	Total     *int64                `json:"total,omitempty" nullable:"true"`
	Values    []WorkspaceFacetValue `json:"values" maxItems:"114"`
	Missing   *int64                `json:"missing,omitempty" nullable:"true"`
	Other     *int64                `json:"other,omitempty" nullable:"true"`
}

// Schema makes all unavailable count fields explicitly nullable rather than
// letting generated clients mistake absent evidence for a successful zero.
func (WorkspaceFacet) Schema(r huma.Registry) *huma.Schema {
	type facetSchema WorkspaceFacet
	schema := huma.SchemaFromType(r, reflect.TypeFor[facetSchema]())
	for _, field := range []string{"total", "missing", "other"} {
		schema.Properties[field] = &huma.Schema{AnyOf: []*huma.Schema{
			{Type: huma.TypeInteger, Minimum: new(float64(0))},
			{Type: "null"},
		}}
	}
	return schema
}

// WorkspaceQueryResponse carries complete frozen metadata and one row page.
type WorkspaceQueryResponse struct {
	Query               QueryPayload               `json:"query"`
	Dependencies        []WorkspaceQueryDependency `json:"dependencies"`
	QueryFingerprint    string                     `json:"query_fingerprint" pattern:"^sha256:[0-9a-f]{64}$"`
	MemberHash          string                     `json:"member_hash" pattern:"^[0-9a-f]{64}$"`
	SnapshotFingerprint string                     `json:"snapshot_fingerprint" pattern:"^sha256:[0-9a-f]{64}$"`
	Generation          WorkspaceQueryGeneration   `json:"generation"`
	Coverage            WorkspaceQueryCoverage     `json:"coverage"`
	ObservedAt          time.Time                  `json:"observed_at" format:"date-time"`
	PageSize            int                        `json:"page_size" enum:"50,100,250"`
	Total               int64                      `json:"total" minimum:"0"`
	TotalBytes          int64                      `json:"total_bytes" minimum:"0"`
	Rows                []WorkspaceQueryRow        `json:"rows" maxItems:"250"`
	Facets              []WorkspaceFacet           `json:"facets" maxItems:"8"`
	Snapshot            bool                       `json:"snapshot"`
	SnapshotID          string                     `json:"snapshot_id" pattern:"^[0-9a-f]{32}$"`
	CreatedAt           time.Time                  `json:"created_at" format:"date-time"`
	ExpiresAt           time.Time                  `json:"expires_at" format:"date-time"`
	PreviousCursor      string                     `json:"previous_cursor,omitempty" maxLength:"2048"`
	NextCursor          string                     `json:"next_cursor,omitempty" maxLength:"2048"`
}

type SavedQueryRunComparison struct {
	HashChanged       bool  `json:"hash_changed"`
	TotalDelta        int64 `json:"total_delta"`
	DefinitionChanged bool  `json:"definition_changed"`
}

type SavedQueryRun struct {
	RunID                    string                  `json:"run_id" format:"uuid"`
	SavedQueryID             string                  `json:"saved_query_id" format:"uuid"`
	SavedQueryRevision       int64                   `json:"saved_query_revision" minimum:"1"`
	QueryFingerprint         string                  `json:"query_fingerprint" pattern:"^sha256:[0-9a-f]{64}$"`
	SnapshotID               string                  `json:"snapshot_id" pattern:"^[0-9a-f]{32}$"`
	MemberHash               string                  `json:"member_hash" pattern:"^[0-9a-f]{64}$"`
	Total                    int64                   `json:"total" minimum:"0"`
	TotalBytes               int64                   `json:"total_bytes" minimum:"0"`
	RanAt                    time.Time               `json:"ran_at" format:"date-time"`
	ExpiresAt                time.Time               `json:"expires_at" format:"date-time"`
	PreviousRunID            string                  `json:"previous_run_id,omitempty" format:"uuid"`
	PreviousMemberHash       string                  `json:"previous_member_hash,omitempty" pattern:"^[0-9a-f]{64}$"`
	PreviousTotal            *int64                  `json:"previous_total,omitempty" nullable:"true" minimum:"0"`
	PreviousQueryFingerprint string                  `json:"previous_query_fingerprint,omitempty" pattern:"^sha256:[0-9a-f]{64}$"`
	Comparison               SavedQueryRunComparison `json:"comparison"`
}

type SavedQueryRunResult struct {
	Run      SavedQueryRun          `json:"run"`
	Snapshot WorkspaceQueryResponse `json:"snapshot"`
}

func fromStoreWorkspacePage(page store.SnapshotPage) (WorkspaceQueryResponse, error) {
	canonical, err := query.Canonical(page.Query)
	if err != nil {
		return WorkspaceQueryResponse{}, err
	}
	out := WorkspaceQueryResponse{
		Query: QueryPayload(canonical), Dependencies: []WorkspaceQueryDependency{},
		QueryFingerprint: page.QueryFingerprint, MemberHash: page.MemberHash,
		SnapshotFingerprint: page.SnapshotFingerprint,
		Generation:          WorkspaceQueryGeneration{Kind: page.Generation.Kind, GenerationID: page.Generation.GenerationID},
		Coverage:            WorkspaceQueryCoverage{Configuration: page.Coverage.Configuration, ProfileFingerprint: page.Coverage.ProfileFingerprint},
		ObservedAt:          page.ObservedAt, PageSize: page.PageSize, Total: page.Total,
		TotalBytes: page.TotalBytes, Rows: []WorkspaceQueryRow{}, Facets: []WorkspaceFacet{},
		Snapshot: page.Snapshot, SnapshotID: page.SnapshotID, CreatedAt: page.CreatedAt,
		ExpiresAt: page.ExpiresAt, PreviousCursor: page.PrevCursor, NextCursor: page.NextCursor,
	}
	for _, dependency := range page.Dependencies {
		out.Dependencies = append(out.Dependencies, WorkspaceQueryDependency{
			Kind: string(dependency.Kind), ID: dependency.ID, Revision: dependency.Revision,
		})
	}
	for _, row := range page.Rows {
		wire := WorkspaceQueryRow{
			NodeID: row.NodeID, ContentVersionID: row.ContentVersionID, BlobHash: row.BlobHash,
			Size: row.Size, Revision: row.Revision, Name: row.Name, Path: row.Path,
			MIMEType: row.MIMEType, MediaFamily: row.MediaFamily, ModifiedAt: row.ModifiedAt,
			SortKey: row.SortKey, Tags: []WorkspaceQueryTag{}, CollectionIDs: row.CollectionIDs,
			DisplayCollectionID: row.DisplayCollectionID, DisplayCollectionLabel: row.DisplayCollectionLabel,
			CoverageState: row.CoverageState, CoverageBuildID: row.CoverageBuildID,
			CoverageAttachmentID: row.CoverageAttachmentID, Excerpt: row.Excerpt,
		}
		for _, tag := range row.Tags {
			wire.Tags = append(wire.Tags, WorkspaceQueryTag{ID: tag.ID, Name: tag.Name, Revision: tag.Revision})
		}
		out.Rows = append(out.Rows, wire)
	}
	for _, facet := range page.Facets {
		wire := WorkspaceFacet{Dimension: facet.Dimension, Available: facet.Available,
			Reason: facet.Reason, Total: facet.Total, Values: []WorkspaceFacetValue{},
			Missing: facet.Missing, Other: facet.Other}
		for _, value := range facet.Values {
			wire.Values = append(wire.Values, WorkspaceFacetValue{
				Key: value.Key, Label: value.Label, Count: value.Count, Selected: value.Selected,
			})
		}
		out.Facets = append(out.Facets, wire)
	}
	return out, nil
}

func fromStoreSavedQueryRun(run store.SavedQueryRun) SavedQueryRun {
	out := SavedQueryRun{
		RunID: run.RunID, SavedQueryID: run.SavedQueryID,
		SavedQueryRevision: run.SavedQueryRevision, QueryFingerprint: run.QueryFingerprint,
		SnapshotID: run.SnapshotID, MemberHash: run.MemberHash, Total: run.Total,
		TotalBytes: run.TotalBytes, RanAt: run.RanAt, ExpiresAt: run.ExpiresAt,
		PreviousRunID: run.PreviousRunID, PreviousMemberHash: run.PreviousMemberHash,
		PreviousQueryFingerprint: run.PreviousQueryFingerprint,
		Comparison: SavedQueryRunComparison{HashChanged: run.Comparison.HashChanged,
			TotalDelta: run.Comparison.TotalDelta, DefinitionChanged: run.Comparison.DefinitionChanged},
	}
	if run.PreviousRunID != "" {
		out.PreviousTotal = new(run.PreviousTotal)
	}
	return out
}
