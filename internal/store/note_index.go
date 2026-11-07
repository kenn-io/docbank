package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"path"
	"slices"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/document/notemarkdown"
)

var ErrNoteIndexIncomplete = errors.New("authored-note index is incomplete")
var ErrNoteIndexCurrent = errors.New("authored-note index is current")

type NoteIndexGeneration struct {
	ID, Publication, DatabaseEpoch, ParserRecipe, ResolverRecipe string
	NamespaceRevision, ExpectedSources                           int64
}
type NoteLookupKey struct{ Kind, Value string }
type NoteLookupCandidate struct {
	Binding, SourceRef, Kind string
	Keys                     []NoteLookupKey
}
type NoteIndexEdge struct {
	Ordinal       int64                  `json:"ordinal"`
	Link          notemarkdown.Link      `json:"link"`
	State         string                 `json:"state"`
	Target        *PublishedNoteIdentity `json:"target,omitempty"`
	TargetHeading *notemarkdown.Span     `json:"target_heading,omitempty"`
}

// readNoteIndexTx fences every staging/read operation against current authority.
// Callers hold either the serialized storage writer or a consistent read tx.
func readNoteIndexTx(ctx context.Context, q metadataQuerier, g NoteIndexGeneration) (sealed, completed bool, err error) {
	if validateUUIDv4(g.ID) != nil || validateUUIDv4(g.DatabaseEpoch) != nil || ValidatePushName(g.Publication) != nil || g.NamespaceRevision < 1 || g.ExpectedSources < 0 || g.ParserRecipe != notemarkdown.Recipe || g.ResolverRecipe != notemarkdown.ResolverRecipe {
		return false, false, ErrInvalidNoteProjection
	}
	var stored NoteIndexGeneration
	stored.ID = g.ID
	err = q.QueryRowContext(ctx, `SELECT g.publication_name,g.database_epoch,g.namespace_revision,g.parser_recipe,g.resolver_recipe,g.expected_sources,g.keys_sealed,g.completed
 FROM note_index_generations g JOIN note_publications p ON p.name=g.publication_name AND p.active=1 AND p.namespace_revision=g.namespace_revision
 JOIN note_publication_state r ON r.singleton=1 AND r.database_epoch=g.database_epoch WHERE g.generation_id=$1`, g.ID).Scan(&stored.Publication, &stored.DatabaseEpoch, &stored.NamespaceRevision, &stored.ParserRecipe, &stored.ResolverRecipe, &stored.ExpectedSources, &sealed, &completed)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, ErrNoteIndexStale
	}
	if err != nil {
		return false, false, err
	}
	if stored != g {
		return false, false, ErrNoteIndexStale
	}
	return sealed, completed, nil
}

func (s *Store) BeginNoteIndex(ctx context.Context, target NoteIndexTarget, parserRecipe, resolverRecipe string) (NoteIndexGeneration, error) {
	if ValidatePushName(target.Name) != nil || validateUUIDv4(target.DatabaseEpoch) != nil || target.NamespaceRevision < 1 || parserRecipe != notemarkdown.Recipe || resolverRecipe != notemarkdown.ResolverRecipe {
		return NoteIndexGeneration{}, ErrInvalidNoteProjection
	}
	g := NoteIndexGeneration{Publication: target.Name, DatabaseEpoch: target.DatabaseEpoch, NamespaceRevision: target.NamespaceRevision, ParserRecipe: parserRecipe, ResolverRecipe: resolverRecipe}
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var epoch string
		var revision int64
		err := tx.QueryRowContext(ctx, `SELECT r.database_epoch,p.namespace_revision FROM note_publications p CROSS JOIN note_publication_state r WHERE p.name=$1 AND p.active=1 AND r.singleton=1`, target.Name).Scan(&epoch, &revision)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoteIndexStale
		}
		if err != nil {
			return err
		}
		if epoch != g.DatabaseEpoch || revision != g.NamespaceRevision {
			return ErrNoteIndexStale
		}
		var current int
		err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM note_index_heads h JOIN note_index_generations g ON g.generation_id=h.generation_id WHERE h.publication_name=$1 AND g.database_epoch=$2 AND g.namespace_revision=$3 AND g.parser_recipe=$4 AND g.resolver_recipe=$5 AND g.completed=1`, g.Publication, g.DatabaseEpoch, g.NamespaceRevision, g.ParserRecipe, g.ResolverRecipe).Scan(&current)
		if err != nil {
			return err
		}
		if current != 0 {
			return ErrNoteIndexCurrent
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM note_index_generations WHERE publication_name=$1 AND generation_id NOT IN (SELECT generation_id FROM note_index_heads)`, g.Publication); err != nil {
			return err
		}
		predicate, args := publishedNoteEligibilitySQLParameter(&PublishedNoteSelector{Publication: g.Publication}, "$1")
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM published_note_sources p JOIN nodes n ON n.id=p.node_id JOIN content_versions cv ON cv.version_id=n.current_version_id WHERE p.publication_name=$1 AND `+predicate, args...).Scan(&g.ExpectedSources); err != nil {
			return err
		}
		g.ID, err = newUUIDv4()
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO note_index_generations(generation_id,publication_name,database_epoch,namespace_revision,parser_recipe,resolver_recipe,expected_sources,keys_sealed,completed) VALUES($1,$2,$3,$4,$5,$6,$7,0,0)`, g.ID, g.Publication, g.DatabaseEpoch, g.NamespaceRevision, g.ParserRecipe, g.ResolverRecipe, g.ExpectedSources)
		return err
	})
	if err != nil {
		return NoteIndexGeneration{}, err
	}
	return g, nil
}

func noteIndexKeys(sourceRef string, note notemarkdown.Note) []NoteLookupKey {
	keys := []NoteLookupKey{{"path", sourceRef}}
	if basename := strings.TrimSuffix(path.Base(sourceRef), path.Ext(sourceRef)); basename != "" {
		keys = append(keys, NoteLookupKey{"basename", basename})
	}
	for _, alias := range note.Metadata.Aliases {
		key := NoteLookupKey{"alias", alias}
		if !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
	}
	return keys
}
func validNoteLookupKey(key NoteLookupKey) bool {
	if key.Kind != "path" && key.Kind != "basename" && key.Kind != "alias" {
		return false
	}
	if len(key.Value) == 0 || len(key.Value) > 4096 || !utf8.ValidString(key.Value) || strings.ContainsRune(key.Value, 0) {
		return false
	}
	return key.Kind != "path" || ValidatePushRef(key.Value) == nil
}
func sameNoteKeys(a, b []NoteLookupKey) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[NoteLookupKey]bool, len(a))
	for _, key := range a {
		if !validNoteLookupKey(key) || seen[key] {
			return false
		}
		seen[key] = true
	}
	for _, key := range b {
		if !seen[key] {
			return false
		}
	}
	return true
}
func parseNoteRecord(record NoteParseRecord) (notemarkdown.Note, error) {
	var note notemarkdown.Note
	if record.State == "ready" {
		if err := json.Unmarshal(record.Artifact, &note); err != nil {
			return note, ErrInvalidNoteProjection
		}
	}
	return note, nil
}

// StageNoteIndexSource witnesses every eligible exact source before key lookup
// is possible. Pending parses cannot masquerade as an alias-free namespace.
func (s *Store) StageNoteIndexSource(ctx context.Context, g NoteIndexGeneration, source NotePublicationSource, record NoteParseRecord, keys []NoteLookupKey) error {
	if len(keys) > 258 {
		return ErrInvalidNoteProjection
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		sealed, completed, err := readNoteIndexTx(ctx, tx, g)
		if err != nil {
			return err
		}
		if sealed || completed {
			return ErrNoteIndexIncomplete
		}
		id := source.Identity
		if id.Publication != g.Publication || id.NamespaceRevision != g.NamespaceRevision || id.VaultID != s.VaultID() {
			return ErrInvalidNoteProjection
		}
		current, err := s.publishedNoteSourceTx(ctx, tx, g.Publication, id.SourceRef)
		if err != nil {
			return err
		}
		if current.Identity != id || current.Presence != "present" || current.Availability != "retained" || current.Conflict != "" {
			return ErrNoteIndexStale
		}
		stored, found, err := noteParseForVersionTx(ctx, tx, id.ContentVersionID, g.ParserRecipe)
		if err != nil {
			return err
		}
		if !found {
			return ErrNoteIndexIncomplete
		}
		if stored.ContentVersionID != record.ContentVersionID || stored.Hash != record.Hash || stored.Size != record.Size || stored.Recipe != record.Recipe || stored.State != record.State || stored.Reason != record.Reason || stored.Checksum != record.Checksum || !slices.Equal(stored.Artifact, record.Artifact) {
			return ErrInvalidNoteProjection
		}
		note, err := parseNoteRecord(stored)
		if err != nil {
			return err
		}
		if !sameNoteKeys(keys, noteIndexKeys(id.SourceRef, note)) {
			return ErrInvalidNoteProjection
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO note_index_sources(generation_id,source_ref,node_id,content_version_id,source_revision,parse_generation_id,edges_done) SELECT $1,$2,$3,$4,$5,generation_id,0 FROM note_parse_bindings WHERE content_version_id=$4 AND recipe=$6 ON CONFLICT(generation_id,source_ref) DO NOTHING`, g.ID, id.SourceRef, id.NodeID, id.ContentVersionID, id.SourceRevision, g.ParserRecipe)
		if err != nil {
			return err
		}
		// One bounded row per key avoids a parameter list that grows with a vault.
		for _, key := range keys {
			if _, err = tx.ExecContext(ctx, `INSERT INTO note_lookup_keys(generation_id,key_kind,key_value,source_ref) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, g.ID, key.Kind, key.Value, id.SourceRef); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) SealNoteIndexKeys(ctx context.Context, g NoteIndexGeneration) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		_, _, err := readNoteIndexTx(ctx, tx, g)
		if err != nil {
			return err
		}
		var count int64
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM note_index_sources WHERE generation_id=$1 AND parse_generation_id IS NOT NULL`, g.ID).Scan(&count); err != nil {
			return err
		}
		if count != g.ExpectedSources {
			return ErrNoteIndexIncomplete
		}
		_, err = tx.ExecContext(ctx, `UPDATE note_index_generations SET keys_sealed=1 WHERE generation_id=$1`, g.ID)
		return err
	})
}

func noteIndexBinding(g NoteIndexGeneration, ref, version string) string {
	return publicationRequestDigest(struct{ Publication, SourceRef, Version string }{g.Publication, ref, version})
}

// NoteIndexCandidates returns at most two bindings per requested key, enough
// to establish ambiguity. Evidence for multiple keys is merged by binding.
func (s *Store) NoteIndexCandidates(ctx context.Context, g NoteIndexGeneration, keys []NoteLookupKey) ([]NoteLookupCandidate, error) {
	if len(keys) > 4 {
		return nil, ErrInvalidNoteProjection
	}
	for _, key := range keys {
		if !validNoteLookupKey(key) {
			return nil, ErrInvalidNoteProjection
		}
	}
	result := make([]NoteLookupCandidate, 0, 8)
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		sealed, _, err := readNoteIndexTx(ctx, tx, g)
		if err != nil {
			return err
		}
		if !sealed {
			return ErrNoteIndexIncomplete
		}
		distinct := make(map[NoteLookupKey]bool, 4)
		byBinding := make(map[string]int, 8)
		for _, key := range keys {
			if distinct[key] {
				continue
			}
			distinct[key] = true
			rows, err := tx.QueryContext(ctx, `SELECT k.source_ref,s.content_version_id FROM note_lookup_keys k JOIN note_index_sources s ON s.generation_id=k.generation_id AND s.source_ref=k.source_ref WHERE k.generation_id=$1 AND k.key_kind=$2 AND k.key_value=$3 ORDER BY k.source_ref LIMIT 2`, g.ID, key.Kind, key.Value)
			if err != nil {
				return err
			}
			for rows.Next() {
				var ref, version string
				if err = rows.Scan(&ref, &version); err != nil {
					_ = rows.Close()
					return err
				}
				binding := noteIndexBinding(g, ref, version)
				index, found := byBinding[binding]
				if !found {
					index = len(result)
					byBinding[binding] = index
					result = append(result, NoteLookupCandidate{Binding: binding, SourceRef: ref, Kind: "note"})
				}
				result[index].Keys = append(result[index].Keys, key)
			}
			err = errors.Join(rows.Err(), rows.Close())
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(result, func(a, b NoteLookupCandidate) int { return strings.Compare(a.SourceRef, b.SourceRef) })
	for i := range result {
		slices.SortFunc(result[i].Keys, func(a, b NoteLookupKey) int {
			if c := strings.Compare(a.Kind, b.Kind); c != 0 {
				return c
			}
			return strings.Compare(a.Value, b.Value)
		})
	}
	return result, nil
}

func (s *Store) noteIndexSourceTx(ctx context.Context, q metadataQuerier, g NoteIndexGeneration, ref string) (PublishedNoteIdentity, bool, error) {
	id := PublishedNoteIdentity{VaultID: s.VaultID(), Publication: g.Publication, SourceRef: ref, NamespaceRevision: g.NamespaceRevision}
	var done bool
	err := q.QueryRowContext(ctx, `SELECT s.node_id,s.content_version_id,cv.blob_hash,cv.size,s.source_revision,s.edges_done FROM note_index_sources s JOIN content_versions cv ON cv.version_id=s.content_version_id WHERE s.generation_id=$1 AND s.source_ref=$2`, g.ID, ref).Scan(&id.NodeID, &id.ContentVersionID, &id.Hash, &id.Size, &id.SourceRevision, &done)
	if errors.Is(err, sql.ErrNoRows) {
		return id, false, ErrInvalidNoteProjection
	}
	return id, done, err
}

func validNoteEdgeState(state string) bool {
	switch state {
	case "resolved", "missing", "ambiguous", "pending", "attachment", "external", "invalid", "unsupported", "unavailable":
		return true
	}
	return false
}

// StageNoteIndexEdges checks original occurrence identity, target version and
// heading evidence before storing one complete edge pass for this source.
func (s *Store) StageNoteIndexEdges(ctx context.Context, g NoteIndexGeneration, sourceRef string, edges []NoteIndexEdge) error {
	if ValidatePushRef(sourceRef) != nil || len(edges) > 8192 {
		return ErrInvalidNoteProjection
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		sealed, completed, err := readNoteIndexTx(ctx, tx, g)
		if err != nil {
			return err
		}
		if !sealed {
			return ErrNoteIndexIncomplete
		}
		id, done, err := s.noteIndexSourceTx(ctx, tx, g, sourceRef)
		if err != nil {
			return err
		}
		record, found, err := noteParseForVersionTx(ctx, tx, id.ContentVersionID, g.ParserRecipe)
		if err != nil {
			return err
		}
		if !found {
			return ErrNoteIndexIncomplete
		}
		note, err := parseNoteRecord(record)
		if err != nil {
			return err
		}
		if len(edges) != len(note.Links) {
			return ErrNoteIndexIncomplete
		}
		if completed && !done {
			return ErrNoteIndexIncomplete
		}
		// Cache at most one target artifact. A source may name thousands of targets.
		var targetVersion string
		var targetNote notemarkdown.Note
		for i, edge := range edges {
			if edge.Ordinal != int64(i) || edge.Link != note.Links[i] || !validNoteEdgeState(edge.State) {
				return ErrInvalidNoteProjection
			}
			if edge.State == "resolved" && edge.Target == nil || edge.Target == nil && edge.TargetHeading != nil {
				return ErrInvalidNoteProjection
			}
			targetRef := ""
			if edge.Target != nil {
				target, _, err := s.noteIndexSourceTx(ctx, tx, g, edge.Target.SourceRef)
				if err != nil {
					return err
				}
				if target != *edge.Target {
					return ErrInvalidNoteProjection
				}
				targetRef = target.SourceRef
				if edge.TargetHeading != nil {
					if targetVersion != target.ContentVersionID {
						targetRecord, found, err := noteParseForVersionTx(ctx, tx, target.ContentVersionID, g.ParserRecipe)
						if err != nil {
							return err
						}
						if !found || targetRecord.State != "ready" {
							return ErrInvalidNoteProjection
						}
						targetNote, err = parseNoteRecord(targetRecord)
						if err != nil {
							return err
						}
						targetVersion = target.ContentVersionID
					}
					valid := false
					for _, heading := range targetNote.Headings {
						if heading.Span == *edge.TargetHeading {
							valid = true
							break
						}
					}
					if !valid {
						return ErrInvalidNoteProjection
					}
				}
			}
			data, err := json.Marshal(edge)
			if err != nil {
				return err
			}
			if len(data) > 64<<10 {
				return ErrInvalidNoteProjection
			}
			if done {
				var stored []byte
				if err = tx.QueryRowContext(ctx, `SELECT edge_json FROM note_link_edges WHERE generation_id=$1 AND source_ref=$2 AND ordinal=$3`, g.ID, sourceRef, i).Scan(&stored); err != nil {
					return err
				}
				if !slices.Equal(stored, data) {
					return ErrInvalidNoteProjection
				}
			} else {
				if _, err = tx.ExecContext(ctx, `INSERT INTO note_link_edges(generation_id,source_ref,ordinal,target_source_ref,edge_state,edge_json) VALUES($1,$2,$3,$4,$5,$6)`, g.ID, sourceRef, i, targetRef, edge.State, data); err != nil {
					return err
				}
			}
		}
		if done {
			return nil
		}
		_, err = tx.ExecContext(ctx, `UPDATE note_index_sources SET edges_done=1 WHERE generation_id=$1 AND source_ref=$2`, g.ID, sourceRef)
		return err
	})
}

func (s *Store) PublishNoteIndex(ctx context.Context, g NoteIndexGeneration) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		sealed, _, err := readNoteIndexTx(ctx, tx, g)
		if err != nil {
			return err
		}
		if !sealed {
			return ErrNoteIndexIncomplete
		}
		var count int64
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM note_index_sources WHERE generation_id=$1 AND edges_done=1 AND parse_generation_id IS NOT NULL`, g.ID).Scan(&count); err != nil {
			return err
		}
		if count != g.ExpectedSources {
			return ErrNoteIndexIncomplete
		}
		if _, err = tx.ExecContext(ctx, `UPDATE note_index_generations SET completed=1 WHERE generation_id=$1`, g.ID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO note_index_heads(publication_name,generation_id) VALUES($1,$2) ON CONFLICT(publication_name) DO UPDATE SET generation_id=excluded.generation_id`, g.Publication, g.ID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM note_index_generations WHERE publication_name=$1 AND generation_id<>$2`, g.Publication, g.ID)
		return err
	})
}
