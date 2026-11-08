package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"

	"go.kenn.io/docbank/document/notemarkdown"
)

var ErrNoteContextStale = errors.New("note context continuation is stale")
var ErrNoteGraphUnavailable = errors.New("note graph is unavailable for this version")

// NoteContextPosition is a generation-bound keyset position. API callers must
// authenticate it before passing it to the store.
type NoteContextPosition struct {
	GenerationID, DatabaseEpoch, SourceRef string
	NamespaceRevision, Ordinal             int64
}

type NoteContextRequest struct {
	Publication, SourceRef, ContentVersionID, Direction string
	Limit                                               int
	After                                               *NoteContextPosition
}

type NoteContextEdge struct {
	Source PublishedNoteIdentity
	Edge   NoteIndexEdge
}

type NoteContextPage struct {
	Source     NotePublicationSource
	Status     NotePublicationStatus
	Parse      NoteParseRecord
	ParseFound bool
	GraphReady bool
	Generation NoteIndexGeneration
	Edges      []NoteContextEdge
	Next       *NoteContextPosition
}

// NoteContext reads one current publication source and one bounded edge page
// from the same snapshot. A continuation never crosses a generation change.
func (s *Store) NoteContext(ctx context.Context, request NoteContextRequest) (NoteContextPage, error) {
	var page NoteContextPage
	if ValidatePushName(request.Publication) != nil || ValidatePushRef(request.SourceRef) != nil ||
		request.Limit < 1 || request.Limit > 50 ||
		(request.Direction != "outgoing" && request.Direction != "incoming" && request.Direction != "unresolved") ||
		(request.ContentVersionID != "" && validateUUIDv4(request.ContentVersionID) != nil) {
		return page, ErrInvalidNoteProjection
	}
	if request.After != nil && (validateUUIDv4(request.After.GenerationID) != nil || validateUUIDv4(request.After.DatabaseEpoch) != nil ||
		request.After.NamespaceRevision < 1 || request.After.Ordinal < 0 || ValidatePushRef(request.After.SourceRef) != nil) {
		return page, ErrNoteContextStale
	}
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		page.Status, err = notePublicationStatusTx(ctx, tx, request.Publication)
		if err != nil {
			return err
		}
		if request.After != nil && (request.After.DatabaseEpoch != page.Status.DatabaseEpoch || request.After.NamespaceRevision != page.Status.NamespaceRevision) {
			return ErrNoteContextStale
		}
		page.Source, err = s.publishedNoteSourceTx(ctx, tx, request.Publication, request.SourceRef)
		if err != nil {
			return err
		}
		if page.Source.Availability == noteSourceUnavailable {
			return ErrNoteSourceUnavailable
		}
		if page.Source.Presence != "present" {
			return ErrNotFound
		}
		if page.Source.Conflict != "" {
			return ErrPublishedSourceDiverged
		}
		if request.ContentVersionID != "" && request.ContentVersionID != page.Source.Identity.ContentVersionID {
			return ErrNoteGraphUnavailable
		}
		page.Parse, page.ParseFound, err = noteParseForVersionTx(ctx, tx, page.Source.Identity.ContentVersionID, notemarkdown.Recipe)
		if err != nil {
			return err
		}
		g := &page.Generation
		err = tx.QueryRowContext(ctx, `SELECT g.generation_id,g.publication_name,g.database_epoch,g.namespace_revision,g.parser_recipe,g.resolver_recipe,g.expected_sources
 FROM note_index_heads h JOIN note_index_generations g ON g.generation_id=h.generation_id
 WHERE h.publication_name=$1 AND g.completed=1 AND g.database_epoch=$2 AND g.namespace_revision=$3 AND g.parser_recipe=$4 AND g.resolver_recipe=$5`,
			request.Publication, page.Status.DatabaseEpoch, page.Status.NamespaceRevision, notemarkdown.Recipe, notemarkdown.ResolverRecipe).
			Scan(&g.ID, &g.Publication, &g.DatabaseEpoch, &g.NamespaceRevision, &g.ParserRecipe, &g.ResolverRecipe, &g.ExpectedSources)
		if errors.Is(err, sql.ErrNoRows) {
			if request.After != nil {
				return ErrNoteContextStale
			}
			return nil
		}
		if err != nil {
			return err
		}
		if request.After != nil && request.After.GenerationID != g.ID {
			return ErrNoteContextStale
		}
		page.GraphReady = true
		return s.noteContextEdgesTx(ctx, tx, request, &page)
	})
	if err != nil {
		return NoteContextPage{}, err
	}
	return page, nil
}

func (s *Store) noteContextEdgesTx(ctx context.Context, tx *sql.Tx, request NoteContextRequest, page *NoteContextPage) error {
	filter := "e.source_ref=$2"
	if request.Direction == "incoming" {
		filter = "e.target_source_ref=$2"
	} else if request.Direction == "unresolved" {
		filter += " AND e.edge_state NOT IN ('resolved','external','attachment')"
	}
	afterRef, afterOrdinal := "", int64(-1)
	if request.After != nil {
		afterRef, afterOrdinal = request.After.SourceRef, request.After.Ordinal
	}
	rows, err := tx.QueryContext(ctx, `SELECT e.source_ref,e.ordinal,length(e.edge_json),
 CASE WHEN length(e.edge_json)<=65536 THEN e.edge_json ELSE NULL END,
 p.node_id,p.content_version_id,p.blob_hash,p.size,p.source_revision
 FROM note_link_edges e JOIN published_note_sources p ON p.publication_name=$3 AND p.source_ref=e.source_ref
 WHERE e.generation_id=$1 AND `+filter+` AND (e.source_ref>$4 OR (e.source_ref=$4 AND e.ordinal>$5))
 ORDER BY e.source_ref,e.ordinal LIMIT $6`, page.Generation.ID, request.SourceRef, request.Publication, afterRef, afterOrdinal, request.Limit+1)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	page.Edges = make([]NoteContextEdge, 0, request.Limit)
	for rows.Next() {
		var item NoteContextEdge
		var ref string
		var ordinal, length int64
		var raw []byte
		if err := rows.Scan(&ref, &ordinal, &length, &raw, &item.Source.NodeID, &item.Source.ContentVersionID, &item.Source.Hash, &item.Source.Size, &item.Source.SourceRevision); err != nil {
			return err
		}
		if length < 1 || length > 65536 || int64(len(raw)) != length {
			return ErrInvalidNoteProjection
		}
		if err := json.Unmarshal(raw, &item.Edge, json.RejectUnknownMembers(true)); err != nil || item.Edge.Ordinal != ordinal {
			return ErrInvalidNoteProjection
		}
		item.Source.VaultID, item.Source.Publication, item.Source.SourceRef = s.VaultID(), request.Publication, ref
		item.Source.NamespaceRevision = page.Status.NamespaceRevision
		if len(page.Edges) == request.Limit {
			last := page.Edges[len(page.Edges)-1]
			page.Next = &NoteContextPosition{GenerationID: page.Generation.ID, DatabaseEpoch: page.Status.DatabaseEpoch,
				NamespaceRevision: page.Status.NamespaceRevision, SourceRef: last.Source.SourceRef, Ordinal: last.Edge.Ordinal}
			break
		}
		page.Edges = append(page.Edges, item)
	}
	return rows.Err()
}
