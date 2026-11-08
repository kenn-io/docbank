// Package notebook derives authored-note context from immutable original bytes.
package notebook

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"io"

	"go.kenn.io/docbank/document/notemarkdown"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/packstore"
)

const sourcePage = 64
const maxOriginalBytes = 4 << 20

var errOriginal = errors.New("authored-note original could not be verified")

type noteCatalog interface {
	NotePublicationStatus(ctx context.Context, _ string) (store.NotePublicationStatus, error)
	EligibleNoteSources(ctx context.Context, _ string, _ string, _ int) ([]store.NotePublicationSource, error)
	NoteParseForVersion(ctx context.Context, _ string, _ string) (store.NoteParseRecord, bool, error)
	RecordNoteParse(ctx context.Context, _ store.NoteParseRecord) error
	BeginNoteIndex(ctx context.Context, _ store.NoteIndexTarget, _ string, _ string) (store.NoteIndexGeneration, error)
	StageNoteIndexSource(ctx context.Context, _ store.NoteIndexGeneration, _ store.NotePublicationSource, _ store.NoteParseRecord, _ []store.NoteLookupKey) error
	SealNoteIndexKeys(ctx context.Context, _ store.NoteIndexGeneration) error
	NoteIndexCandidates(ctx context.Context, _ store.NoteIndexGeneration, _ []store.NoteLookupKey) ([]store.NoteLookupCandidate, error)
	NoteIndexSource(ctx context.Context, _ store.NoteIndexGeneration, _ string) (store.PublishedNoteIdentity, store.NoteParseRecord, error)
	StageNoteIndexEdges(ctx context.Context, _ store.NoteIndexGeneration, _ string, _ []store.NoteIndexEdge) error
	PublishNoteIndex(ctx context.Context, _ store.NoteIndexGeneration) error
	NoteIndexTargets(ctx context.Context, _ string, _ int) ([]store.NoteIndexTarget, error)
	WriteRevision() uint64
}

type originalReader interface {
	OpenStreamContext(ctx context.Context, _ string) (packstore.VerifiedReadCloser, int64, error)
}

// Projector builds one fenced namespace generation with bounded source pages.
type Projector struct {
	catalog noteCatalog
	blobs   originalReader
}

// NewProjector uses the daemon's existing catalog and immutable blob store.
func NewProjector(catalog *store.Store, blobs *blob.Store) *Projector {
	return &Projector{catalog: catalog, blobs: blobs}
}

// Process parses exact accepted originals, seals every lookup key, then resolves
// authored occurrences against that complete namespace before publishing it.
func (p *Projector) Process(ctx context.Context, target store.NoteIndexTarget) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	initial, err := p.catalog.NotePublicationStatus(ctx, target.Name)
	if err != nil {
		return err
	}
	if initial.DatabaseEpoch != target.DatabaseEpoch || initial.NamespaceRevision != target.NamespaceRevision {
		return store.ErrNoteIndexStale
	}
	if err = p.sources(ctx, target.Name, func(source store.NotePublicationSource) error {
		return p.parse(ctx, source.Identity)
	}); err != nil {
		return err
	}
	// Adding exact-version parse bindings advances the namespace itself. Start
	// graph staging at the resulting revision, but never cross a restore epoch.
	current, err := p.catalog.NotePublicationStatus(ctx, target.Name)
	if err != nil {
		return err
	}
	if current.DatabaseEpoch != initial.DatabaseEpoch {
		return store.ErrNoteIndexStale
	}
	target.NamespaceRevision = current.NamespaceRevision
	g, err := p.catalog.BeginNoteIndex(ctx, target, notemarkdown.Recipe, notemarkdown.ResolverRecipe)
	if errors.Is(err, store.ErrNoteIndexCurrent) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = p.sources(ctx, target.Name, func(source store.NotePublicationSource) error {
		record, found, err := p.catalog.NoteParseForVersion(ctx, source.Identity.ContentVersionID, notemarkdown.Recipe)
		if err != nil {
			return err
		}
		if !found {
			status, err := p.catalog.NotePublicationStatus(ctx, g.Publication)
			if err != nil {
				return err
			}
			if status.DatabaseEpoch != g.DatabaseEpoch || status.NamespaceRevision != g.NamespaceRevision {
				return store.ErrNoteIndexStale
			}
			return store.ErrNoteIndexIncomplete
		}
		note, err := parseArtifact(record)
		if err != nil {
			return err
		}
		return p.catalog.StageNoteIndexSource(ctx, g, source, record, store.NoteIndexKeys(source.Identity.SourceRef, note))
	}); err != nil {
		return err
	}
	if err = p.catalog.SealNoteIndexKeys(ctx, g); err != nil {
		return err
	}
	if err = p.sources(ctx, target.Name, func(source store.NotePublicationSource) error {
		identity, record, err := p.catalog.NoteIndexSource(ctx, g, source.Identity.SourceRef)
		if err != nil {
			return err
		}
		note, err := parseArtifact(record)
		if err != nil {
			return err
		}
		edges, err := p.edges(ctx, g, identity.SourceRef, note.Links)
		if err != nil {
			return err
		}
		return p.catalog.StageNoteIndexEdges(ctx, g, identity.SourceRef, edges)
	}); err != nil {
		return err
	}
	return p.catalog.PublishNoteIndex(ctx, g)
}

func (p *Projector) sources(ctx context.Context, publication string, visit func(store.NotePublicationSource) error) error {
	after := ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		page, err := p.catalog.EligibleNoteSources(ctx, publication, after, sourcePage)
		if err != nil {
			return err
		}
		for _, source := range page {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := visit(source); err != nil {
				return err
			}
		}
		if len(page) < sourcePage {
			return nil
		}
		after = page[len(page)-1].Identity.SourceRef
	}
}

func (p *Projector) parse(ctx context.Context, id store.PublishedNoteIdentity) error {
	_, found, err := p.catalog.NoteParseForVersion(ctx, id.ContentVersionID, notemarkdown.Recipe)
	if err != nil || found {
		return err
	}
	record := store.NoteParseRecord{ContentVersionID: id.ContentVersionID, Hash: id.Hash, Size: id.Size, Recipe: notemarkdown.Recipe}
	if id.Size > maxOriginalBytes {
		record.State, record.Reason = "limit", "input_limit"
		return p.catalog.RecordNoteParse(ctx, record)
	}
	data, err := p.original(ctx, id)
	if err != nil {
		return err
	}
	note, err := notemarkdown.Parse(ctx, data)
	switch {
	case errors.Is(err, notemarkdown.ErrLimit):
		record.State, record.Reason = "limit", "parser_limit"
	case errors.Is(err, notemarkdown.ErrInvalidInput):
		record.State, record.Reason = "failed", "invalid_markdown"
	case err != nil:
		return err
	default:
		record.Artifact, err = notemarkdown.CanonicalArtifact(ctx, note)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(record.Artifact)
		record.State, record.Checksum = "ready", hex.EncodeToString(sum[:])
	}
	return p.catalog.RecordNoteParse(ctx, record)
}

func (p *Projector) original(ctx context.Context, id store.PublishedNoteIdentity) ([]byte, error) {
	stream, size, err := p.blobs.OpenStreamContext(ctx, id.Hash)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errOriginal
	}
	if size != id.Size {
		_ = stream.Close()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, errOriginal
	}
	data, readErr := io.ReadAll(io.LimitReader(stream, maxOriginalBytes+1))
	sum := sha256.Sum256(data)
	if readErr != nil || int64(len(data)) != id.Size || hex.EncodeToString(sum[:]) != id.Hash {
		// Verify may drain an unfinished stream. Once admission has failed,
		// close it directly so verification cannot defeat the read bound.
		_ = stream.Close()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, errOriginal
	}
	verifyErr := stream.Verify()
	closeErr := stream.Close()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if verifyErr != nil || closeErr != nil {
		return nil, errOriginal
	}
	return data, nil
}

func parseArtifact(record store.NoteParseRecord) (notemarkdown.Note, error) {
	var note notemarkdown.Note
	if record.State == "ready" {
		if err := json.Unmarshal(record.Artifact, &note); err != nil {
			return note, err
		}
	}
	return note, nil
}

func (p *Projector) edges(ctx context.Context, g store.NoteIndexGeneration, sourceRef string, links []notemarkdown.Link) ([]store.NoteIndexEdge, error) {
	edges := make([]store.NoteIndexEdge, 0, len(links))
	// A source may name thousands of targets. Retain only the last exact target.
	lastRef := ""
	var lastIdentity store.PublishedNoteIdentity
	var lastRecord store.NoteParseRecord
	var lastNote notemarkdown.Note
	for i, link := range links {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		query, err := notemarkdown.PlanTarget(sourceRef, link)
		if err != nil {
			if errors.Is(err, notemarkdown.ErrLimit) {
				edges = append(edges, store.NoteIndexEdge{Ordinal: int64(i), Link: link, State: "unsupported"})
				continue
			}
			return nil, err
		}
		keys := make([]store.NoteLookupKey, 0, len(query.Keys))
		for _, key := range query.Keys {
			keys = append(keys, store.NoteLookupKey{Kind: key.Kind, Value: key.Value})
		}
		rows, err := p.catalog.NoteIndexCandidates(ctx, g, keys)
		if err != nil {
			return nil, err
		}
		set := notemarkdown.CandidateSet{NamespaceGeneration: g.ID, Complete: true}
		for _, row := range rows {
			candidate := notemarkdown.Candidate{Binding: row.Binding, Path: row.SourceRef, Kind: row.Kind}
			for _, key := range row.Keys {
				candidate.Keys = append(candidate.Keys, notemarkdown.LookupKey{Kind: key.Kind, Value: key.Value})
			}
			set.Candidates = append(set.Candidates, candidate)
		}
		resolution, err := notemarkdown.Resolve(ctx, query, set)
		if err != nil {
			return nil, err
		}
		edge := store.NoteIndexEdge{Ordinal: int64(i), Link: link, State: resolution.State}
		if resolution.Binding != "" {
			var targetRef string
			for _, row := range rows {
				if row.Binding == resolution.Binding {
					targetRef = row.SourceRef
					break
				}
			}
			if targetRef == "" {
				return nil, store.ErrInvalidNoteProjection
			}
			if targetRef != lastRef {
				lastIdentity, lastRecord, err = p.catalog.NoteIndexSource(ctx, g, targetRef)
				if err != nil {
					return nil, err
				}
				lastNote, err = parseArtifact(lastRecord)
				if err != nil {
					return nil, err
				}
				lastRef = targetRef
			}
			identity := lastIdentity
			edge.Target = &identity
			if query.Fragment != "" {
				if lastRecord.State != "ready" {
					edge.State = "unavailable"
				} else {
					fragment, err := notemarkdown.ResolveFragment(ctx, query.Fragment, lastNote.Headings)
					if err != nil {
						return nil, err
					}
					edge.State, edge.TargetHeading = fragment.State, fragment.Heading
				}
			}
		}
		edges = append(edges, edge)
	}
	return edges, nil
}
