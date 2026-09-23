package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/query"
)

const maxConceptDescriptionBytes = 4096
const maxConceptGraphNodes = 1000
const maxTagMergeRefs = 1000

// TagConcept adds optional vocabulary to an existing tag UUID. Its revision
// fences vocabulary edits separately from document assignment revisions.
type TagConcept struct {
	TagID       string   `json:"tag_id"`
	Description string   `json:"description"`
	Revision    int64    `json:"revision"`
	Aliases     []string `json:"aliases"`
	NarrowerIDs []string `json:"narrower_ids"`
	BroaderIDs  []string `json:"broader_ids"`
	RelatedIDs  []string `json:"related_ids"`
}

func conceptByTagIDTx(ctx context.Context, tx *sql.Tx, tagID string) (TagConcept, error) {
	if _, err := tagByIDTx(tx, tagID); err != nil {
		return TagConcept{}, err
	}
	result := TagConcept{TagID: tagID, Revision: 1, Aliases: []string{},
		NarrowerIDs: []string{}, BroaderIDs: []string{}, RelatedIDs: []string{}}
	err := tx.QueryRowContext(ctx, `SELECT description,revision FROM tag_concepts WHERE tag_id=?`, tagID).
		Scan(&result.Description, &result.Revision)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return TagConcept{}, err
	}
	result.Aliases, err = conceptAliasesTx(ctx, tx, tagID)
	if err != nil {
		return TagConcept{}, err
	}
	err = conceptEdgesTx(ctx, tx, tagID, &result)
	if err != nil {
		return TagConcept{}, err
	}
	sort.Strings(result.NarrowerIDs)
	sort.Strings(result.BroaderIDs)
	sort.Strings(result.RelatedIDs)
	return result, nil
}

func conceptAliasesTx(ctx context.Context, tx *sql.Tx, tagID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT alias FROM tag_aliases WHERE tag_id=? ORDER BY alias`, tagID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []string{}
	for rows.Next() {
		var alias string
		if err := rows.Scan(&alias); err != nil {
			return nil, err
		}
		result = append(result, alias)
	}
	return result, rows.Err()
}

func conceptEdgesTx(ctx context.Context, tx *sql.Tx, tagID string, result *TagConcept) error {
	rows, err := tx.QueryContext(ctx, `SELECT parent_tag_id,child_tag_id,kind FROM tag_concept_edges
		WHERE parent_tag_id=? OR child_tag_id=? ORDER BY parent_tag_id,child_tag_id,kind`, tagID, tagID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var parent, child, kind string
		if err := rows.Scan(&parent, &child, &kind); err != nil {
			return err
		}
		switch kind {
		case "broader":
			if parent == tagID {
				result.NarrowerIDs = append(result.NarrowerIDs, child)
			} else {
				result.BroaderIDs = append(result.BroaderIDs, parent)
			}
		case "related":
			if parent == tagID {
				result.RelatedIDs = append(result.RelatedIDs, child)
			} else {
				result.RelatedIDs = append(result.RelatedIDs, parent)
			}
		default:
			return fmt.Errorf("unknown concept edge kind %q", kind)
		}
	}
	return rows.Err()
}

func (s *Store) ConceptByTagID(ctx context.Context, tagID string) (TagConcept, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return TagConcept{}, err
	}
	defer func() { _ = tx.Rollback() }()
	concept, err := conceptByTagIDTx(ctx, tx, tagID)
	if err != nil {
		return TagConcept{}, err
	}
	return concept, tx.Commit()
}

func ensureConceptTx(ctx context.Context, tx *sql.Tx, tagID string) error {
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO tag_concepts(tag_id,description,revision)
		VALUES(?,'',1)`, tagID)
	return err
}

func checkConceptRevision(concept TagConcept, ifRev int64) error {
	if concept.Revision != ifRev {
		return fmt.Errorf("concept %s revision %d, expected %d: %w",
			concept.TagID, concept.Revision, ifRev, ErrStaleRevision)
	}
	return nil
}

func validConceptDescription(value string) bool {
	return utf8.ValidString(value) && len(value) <= maxConceptDescriptionBytes &&
		!strings.ContainsRune(value, 0)
}

func (s *Store) SetTagConcept(ctx context.Context, tagID string, ifRev int64, description string) (TagConcept, error) {
	if !validConceptDescription(description) {
		return TagConcept{}, ErrInvalidTag
	}
	var result TagConcept
	err := s.withConceptTx(ctx, func(tx *sql.Tx) error {
		current, err := conceptByTagIDTx(ctx, tx, tagID)
		if err != nil {
			return err
		}
		if err := checkConceptRevision(current, ifRev); err != nil {
			return err
		}
		if current.Description == description {
			result = current
			return nil
		}
		if err := ensureConceptTx(ctx, tx, tagID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tag_concepts SET description=?,revision=revision+1 WHERE tag_id=?`, description, tagID); err != nil {
			return err
		}
		result, err = conceptByTagIDTx(ctx, tx, tagID)
		return err
	})
	return result, err
}

// ResolveTagConcept resolves an exact canonical name or one unique alias.
// A slash is always a literal part of the name.
func (s *Store) ResolveTagConcept(ctx context.Context, name string) (Tag, error) {
	name, err := NormalizeTagName(name)
	if err != nil {
		return Tag{}, err
	}
	if tag, err := s.TagByName(ctx, name); err == nil {
		return tag, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Tag{}, err
	}
	var tagID string
	if err := s.db.QueryRowContext(ctx, `SELECT tag_id FROM tag_aliases WHERE alias=?`, name).Scan(&tagID); errors.Is(err, sql.ErrNoRows) {
		return Tag{}, ErrNotFound
	} else if err != nil {
		return Tag{}, err
	}
	return s.TagByID(ctx, tagID)
}

func (s *Store) AddTagAlias(ctx context.Context, tagID string, ifRev int64, alias string) (TagConcept, error) {
	alias, err := NormalizeTagName(alias)
	if err != nil {
		return TagConcept{}, err
	}
	var result TagConcept
	err = s.withConceptTx(ctx, func(tx *sql.Tx) error {
		current, err := conceptByTagIDTx(ctx, tx, tagID)
		if err != nil {
			return err
		}
		if err := checkConceptRevision(current, ifRev); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tags WHERE name=?)`, alias).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return ErrExists
		}
		var owner string
		err = tx.QueryRowContext(ctx, `SELECT tag_id FROM tag_aliases WHERE alias=?`, alias).Scan(&owner)
		if err == nil && owner != tagID {
			return ErrExists
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if slices.Contains(current.Aliases, alias) {
			result = current
			return nil
		}
		if err := ensureConceptTx(ctx, tx, tagID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO tag_aliases(alias,tag_id) VALUES(?,?)`, alias, tagID); err != nil {
			if s.driver.IsUniqueViolation(err) {
				return ErrExists
			}
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tag_concepts SET revision=revision+1 WHERE tag_id=?`, tagID); err != nil {
			return err
		}
		result, err = conceptByTagIDTx(ctx, tx, tagID)
		return err
	})
	return result, err
}

func (s *Store) RemoveTagAlias(ctx context.Context, tagID string, ifRev int64, alias string) (TagConcept, error) {
	alias, err := NormalizeTagName(alias)
	if err != nil {
		return TagConcept{}, err
	}
	var result TagConcept
	err = s.withConceptTx(ctx, func(tx *sql.Tx) error {
		current, err := conceptByTagIDTx(ctx, tx, tagID)
		if err != nil {
			return err
		}
		if err := checkConceptRevision(current, ifRev); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM tag_aliases WHERE alias=? AND tag_id=?`, alias, tagID)
		if err != nil {
			return err
		}
		changed, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if changed > 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE tag_concepts SET revision=revision+1 WHERE tag_id=?`, tagID); err != nil {
				return err
			}
		}
		result, err = conceptByTagIDTx(ctx, tx, tagID)
		return err
	})
	return result, err
}

func (s *Store) AddConceptEdge(ctx context.Context, parentID, childID, kind string) error {
	if parentID == childID {
		return ErrCycle
	}
	if kind != "broader" && kind != "related" {
		return ErrInvalidTag
	}
	if kind == "related" && childID < parentID {
		parentID, childID = childID, parentID
	}
	return s.withConceptTx(ctx, func(tx *sql.Tx) error {
		for _, id := range []string{parentID, childID} {
			if _, err := tagByIDTx(tx, id); err != nil {
				return err
			}
		}
		if kind == "broader" {
			rows, err := tx.QueryContext(ctx, `SELECT parent_tag_id,child_tag_id FROM tag_concept_edges WHERE kind='broader'`)
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			edges := map[string][]string{}
			edgeCount := 0
			for rows.Next() {
				var parent, child string
				if err = rows.Scan(&parent, &child); err != nil {
					break
				}
				edges[parent] = append(edges[parent], child)
				edgeCount++
				if edgeCount > maxConceptGraphNodes {
					err = ErrInvalidTag
					break
				}
			}
			if err == nil {
				err = rows.Err()
			}
			_ = rows.Close()
			if err != nil {
				return err
			}
			if document.WouldCreateConceptCycle(edges, parentID, childID) {
				return ErrCycle
			}
		}
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO tag_concept_edges(parent_tag_id,child_tag_id,kind) VALUES(?,?,?)`, parentID, childID, kind)
		if err != nil {
			return err
		}
		changed, err := res.RowsAffected()
		if err != nil || changed == 0 {
			return err
		}
		for _, id := range []string{parentID, childID} {
			if err := ensureConceptTx(ctx, tx, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE tag_concepts SET revision=revision+1 WHERE tag_id=?`, id); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) RemoveConceptEdge(ctx context.Context, parentID, childID, kind string) error {
	if kind != "broader" && kind != "related" {
		return ErrInvalidTag
	}
	if kind == "related" && childID < parentID {
		parentID, childID = childID, parentID
	}
	return s.withConceptTx(ctx, func(tx *sql.Tx) error {
		for _, id := range []string{parentID, childID} {
			if _, err := tagByIDTx(tx, id); err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM tag_concept_edges
			WHERE parent_tag_id=? AND child_tag_id=? AND kind=?`, parentID, childID, kind)
		if err != nil {
			return err
		}
		changed, err := res.RowsAffected()
		if err != nil || changed == 0 {
			return err
		}
		for _, id := range []string{parentID, childID} {
			if err := ensureConceptTx(ctx, tx, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE tag_concepts SET revision=revision+1 WHERE tag_id=?`, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// TagMergePreview is a revision-fenced inventory of authority a merge would
// move. No identity merge is inferred from a name, including an alias.
type TagMergePreview struct {
	SourceTagID               string   `json:"source_tag_id"`
	TargetTagID               string   `json:"target_tag_id"`
	SourceName                string   `json:"source_name"`
	TargetName                string   `json:"target_name"`
	SourceDescription         string   `json:"source_description"`
	SourceAliases             []string `json:"source_aliases"`
	SourceRevision            int64    `json:"source_revision"`
	TargetRevision            int64    `json:"target_revision"`
	SourceConceptRev          int64    `json:"source_concept_revision"`
	TargetConceptRev          int64    `json:"target_concept_revision"`
	DocumentAssignments       int      `json:"document_assignments"`
	PassageAssignments        int      `json:"passage_assignments"`
	TargetDocumentAssignments int      `json:"target_document_assignments"`
	TargetPassageAssignments  int      `json:"target_passage_assignments"`
	DocumentNodeIDs           []int64  `json:"document_node_ids"`
	PassageIDs                []string `json:"passage_ids"`
	SavedQueryIDs             []string `json:"saved_query_ids"`
	SavedQueryRevisions       []int64  `json:"saved_query_revisions"`
	UnmigratableSavedQueryIDs []string `json:"unmigratable_saved_query_ids"`
	ContentMapIDs             []string `json:"content_map_ids"`
	ContentMapRevisions       []int64  `json:"content_map_revisions"`
	UnmigratableContentMapIDs []string `json:"unmigratable_content_map_ids"`
}

type TagMergeReceipt struct {
	MergeID     string `json:"merge_id"`
	SourceTagID string `json:"source_tag_id"`
	TargetTagID string `json:"target_tag_id"`
}

func (s *Store) PreviewTagMerge(ctx context.Context, sourceID, targetID string) (TagMergePreview, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return TagMergePreview{}, err
	}
	defer func() { _ = tx.Rollback() }()
	preview, err := previewTagMergeTx(ctx, tx, sourceID, targetID)
	if err != nil {
		return TagMergePreview{}, err
	}
	return preview, tx.Commit()
}

func previewTagMergeTx(ctx context.Context, tx *sql.Tx, sourceID, targetID string) (TagMergePreview, error) {
	if sourceID == targetID {
		return TagMergePreview{}, ErrInvalidTag
	}
	source, err := tagByIDTx(tx, sourceID)
	if err != nil {
		return TagMergePreview{}, err
	}
	target, err := tagByIDTx(tx, targetID)
	if err != nil {
		return TagMergePreview{}, err
	}
	sourceConcept, err := conceptByTagIDTx(ctx, tx, sourceID)
	if err != nil {
		return TagMergePreview{}, err
	}
	targetConcept, err := conceptByTagIDTx(ctx, tx, targetID)
	if err != nil {
		return TagMergePreview{}, err
	}
	result := TagMergePreview{SourceTagID: sourceID, TargetTagID: targetID,
		SourceName: source.Name, TargetName: target.Name,
		SourceDescription: sourceConcept.Description, SourceAliases: sourceConcept.Aliases,
		SourceRevision: source.Revision, TargetRevision: target.Revision,
		SourceConceptRev: sourceConcept.Revision, TargetConceptRev: targetConcept.Revision,
		DocumentNodeIDs: []int64{}, PassageIDs: []string{},
		SavedQueryIDs: []string{}, SavedQueryRevisions: []int64{}, UnmigratableSavedQueryIDs: []string{},
		ContentMapIDs: []string{}, ContentMapRevisions: []int64{}, UnmigratableContentMapIDs: []string{}}
	result.DocumentNodeIDs, err = tagMergeNodeIDsTx(ctx, tx, sourceID)
	if err != nil {
		return TagMergePreview{}, err
	}
	if len(result.DocumentNodeIDs) > maxTagMergeRefs {
		return TagMergePreview{}, ErrInvalidTag
	}
	result.DocumentAssignments = len(result.DocumentNodeIDs)
	result.PassageIDs, err = tagMergePassageIDsTx(ctx, tx, sourceID)
	if err != nil {
		return TagMergePreview{}, err
	}
	if len(result.PassageIDs) > maxTagMergeRefs {
		return TagMergePreview{}, ErrInvalidTag
	}
	result.PassageAssignments = len(result.PassageIDs)
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM node_tags WHERE tag_id=?`, targetID).
		Scan(&result.TargetDocumentAssignments); err != nil {
		return TagMergePreview{}, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM passage_tags WHERE tag_id=?`, targetID).
		Scan(&result.TargetPassageAssignments); err != nil {
		return TagMergePreview{}, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,payload,revision FROM saved_queries WHERE kind=? ORDER BY id`, SavedQueryKindQuery)
	if err != nil {
		return TagMergePreview{}, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		var payload []byte
		var revision int64
		if err = rows.Scan(&id, &payload, &revision); err != nil {
			break
		}
		q, parseErr := query.Parse(payload)
		if parseErr != nil {
			err = parseErr
			break
		}
		advancedReference := false
		if q.Syntax == "advanced" {
			expr, parseErr := query.ParseExpression(q.Text, q.Syntax)
			if parseErr != nil {
				err = parseErr
				break
			}
			advancedReference = tagExpressionReferences(expr, sourceID, source.Name, sourceConcept.Aliases)
		}
		if slicesContain(q.Filters.TagIDs, sourceID) || slicesContain(q.Filters.ExcludeTagIDs, sourceID) || advancedReference {
			result.SavedQueryIDs = append(result.SavedQueryIDs, id)
			result.SavedQueryRevisions = append(result.SavedQueryRevisions, revision)
			if advancedReference {
				result.UnmigratableSavedQueryIDs = append(result.UnmigratableSavedQueryIDs, id)
			}
			if len(result.SavedQueryIDs) > maxTagMergeRefs {
				err = ErrInvalidTag
				break
			}
		}
	}
	if err == nil {
		err = rows.Err()
	}
	if err != nil {
		return TagMergePreview{}, err
	}
	if err := appendTagMergeMapReferences(ctx, tx, sourceID, source.Name, sourceConcept.Aliases, &result); err != nil {
		return TagMergePreview{}, err
	}
	return result, nil
}

func appendTagMergeMapReferences(ctx context.Context, tx *sql.Tx, sourceID, sourceName string, aliases []string, preview *TagMergePreview) error {
	rows, err := tx.QueryContext(ctx, `SELECT id,revision,definition_json FROM content_maps ORDER BY id`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		var revision int64
		var encoded []byte
		if err := rows.Scan(&id, &revision, &encoded); err != nil {
			return err
		}
		var definition ContentMapDefinition
		if len(encoded) > document.MaxContentMapMetadataBytes || json.Unmarshal(encoded, &definition) != nil {
			return ErrInvalidContentMap
		}
		var referenced, unmigratable bool
		for _, section := range definition.Sections {
			if section.Selector == nil {
				continue
			}
			selector := section.Selector
			if slices.Contains(selector.Filters.TagIDs, sourceID) || slices.Contains(selector.Filters.ExcludeTagIDs, sourceID) {
				referenced = true
			}
			if selector.Syntax == "advanced" {
				expr, err := query.ParseExpression(selector.Text, selector.Syntax)
				if err != nil {
					return err
				}
				if tagExpressionReferences(expr, sourceID, sourceName, aliases) {
					referenced, unmigratable = true, true
				}
			}
		}
		if referenced {
			preview.ContentMapIDs = append(preview.ContentMapIDs, id)
			preview.ContentMapRevisions = append(preview.ContentMapRevisions, revision)
			if unmigratable {
				preview.UnmigratableContentMapIDs = append(preview.UnmigratableContentMapIDs, id)
			}
			if len(preview.ContentMapIDs) > maxTagMergeRefs {
				return ErrInvalidTag
			}
		}
	}
	return rows.Err()
}

func tagMergeNodeIDsTx(ctx context.Context, tx *sql.Tx, tagID string) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT node_id FROM node_tags WHERE tag_id=? ORDER BY node_id LIMIT ?`, tagID, maxTagMergeRefs+1)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		result = append(result, id)
	}
	return result, rows.Err()
}

func tagMergePassageIDsTx(ctx context.Context, tx *sql.Tx, tagID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT passage_id FROM passage_tags WHERE tag_id=? ORDER BY passage_id LIMIT ?`, tagID, maxTagMergeRefs+1)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		result = append(result, id)
	}
	return result, rows.Err()
}

func slicesContain(values []string, value string) bool {
	return slices.Contains(values, value)
}

func tagExpressionReferences(expr *query.Expression, id, name string, aliases []string) bool {
	if expr == nil {
		return false
	}
	if expr.Kind == query.ExpressionField && expr.Field == "tag" {
		var hasValue func(*query.Expression) bool
		hasValue = func(node *query.Expression) bool {
			if node == nil {
				return false
			}
			if node.Kind == query.ExpressionTerm || node.Kind == query.ExpressionPhrase {
				return node.Value == id || node.Value == name || slices.Contains(aliases, node.Value)
			}
			return slices.ContainsFunc(node.Children, hasValue)
		}
		return hasValue(expr)
	}
	for _, child := range expr.Children {
		if tagExpressionReferences(child, id, name, aliases) {
			return true
		}
	}
	return false
}

func (s *Store) CommitTagMerge(ctx context.Context, preview TagMergePreview) (TagMergeReceipt, error) {
	var receipt TagMergeReceipt
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		current, err := previewTagMergeTx(ctx, tx, preview.SourceTagID, preview.TargetTagID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(current, preview) {
			return ErrStaleRevision
		}
		audited, err := auditAuthorityActiveTx(ctx, tx)
		if err != nil {
			return err
		}
		recordedAt := nowRFC3339()
		var before []byte
		var priorNodes []Node
		var candidates []auditedTagCandidate
		var scopes []auditScopeState
		if audited {
			_, before, err = captureTagMergeSnapshot(ctx, tx)
			if err != nil {
				return err
			}
			priorNodes, candidates, scopes, err = auditedTagMergeNodesTx(ctx, tx, preview.DocumentNodeIDs)
			if err != nil {
				return err
			}
		}
		targetConcept, err := conceptByTagIDTx(ctx, tx, preview.TargetTagID)
		if err != nil {
			return err
		}
		if preview.SourceDescription != "" && targetConcept.Description != "" &&
			preview.SourceDescription != targetConcept.Description {
			return fmt.Errorf("merge concept descriptions differ; reconcile them before merging: %w", ErrInvalidTag)
		}
		mergedDescription := targetConcept.Description
		if mergedDescription == "" {
			mergedDescription = preview.SourceDescription
		}
		if len(preview.UnmigratableSavedQueryIDs) != 0 || len(preview.UnmigratableContentMapIDs) != 0 {
			return fmt.Errorf("merge has advanced tag references requiring explicit edits: %w", ErrInvalidTag)
		}
		// Rewriting a graph may collapse endpoints. Reject rather than quietly
		// dropping a reviewed hierarchy relation or creating a cycle.
		var incident bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tag_concept_edges
			WHERE parent_tag_id=? OR child_tag_id=?)`, preview.SourceTagID, preview.SourceTagID).Scan(&incident); err != nil {
			return err
		}
		if incident {
			return fmt.Errorf("merge requires a separate hierarchy edit: %w", ErrInvalidTag)
		}
		var sourceName string
		if err := tx.QueryRowContext(ctx, `SELECT name FROM tags WHERE id=?`, preview.SourceTagID).Scan(&sourceName); err != nil {
			return err
		}
		mergeID, err := newUUIDv4()
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(preview)
		if err != nil {
			return err
		}
		if err := touchTaggedNodesTx(tx, preview.SourceTagID, recordedAt); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO node_tags(node_id,tag_id)
			SELECT node_id,? FROM node_tags WHERE tag_id=?`, preview.TargetTagID, preview.SourceTagID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO passage_tags(
			passage_id,tag_id,ref_json,document_uid,content_version_id)
			SELECT passage_id,?,ref_json,document_uid,content_version_id
			FROM passage_tags WHERE tag_id=?`, preview.TargetTagID, preview.SourceTagID); err != nil {
			return err
		}
		for _, queryID := range preview.SavedQueryIDs {
			var payload []byte
			if err := tx.QueryRowContext(ctx, `SELECT payload FROM saved_queries WHERE id=?`, queryID).Scan(&payload); err != nil {
				return err
			}
			q, err := query.Parse(payload)
			if err != nil {
				return err
			}
			for _, values := range [][]string{q.Filters.TagIDs, q.Filters.ExcludeTagIDs} {
				for i := range values {
					if values[i] == preview.SourceTagID {
						values[i] = preview.TargetTagID
					}
				}
			}
			canonical, fingerprint, err := query.CanonicalWithFingerprint(q)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE saved_queries SET payload=?,fingerprint=?,revision=revision+1,updated_at=? WHERE id=?`,
				canonical, fingerprint, nowRFC3339(), queryID); err != nil {
				return err
			}
		}
		for _, mapID := range preview.ContentMapIDs {
			var encoded []byte
			if err := tx.QueryRowContext(ctx, `SELECT definition_json FROM content_maps WHERE id=?`, mapID).Scan(&encoded); err != nil {
				return err
			}
			var definition ContentMapDefinition
			if err := json.Unmarshal(encoded, &definition); err != nil {
				return err
			}
			for i := range definition.Sections {
				selector := definition.Sections[i].Selector
				if selector == nil {
					continue
				}
				for _, values := range [][]string{selector.Filters.TagIDs, selector.Filters.ExcludeTagIDs} {
					for j := range values {
						if values[j] == preview.SourceTagID {
							values[j] = preview.TargetTagID
						}
					}
				}
			}
			plan, canonical, err := normalizedMapDefinition(definition)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE content_maps SET definition_json=?,definition_digest=?,revision=revision+1,updated_at=? WHERE id=?`,
				canonical, plan.DefinitionDigest, nowRFC3339(), mapID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tag_aliases SET tag_id=? WHERE tag_id=?`, preview.TargetTagID, preview.SourceTagID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tag_redirects SET target_tag_id=? WHERE target_tag_id=?`,
			preview.TargetTagID, preview.SourceTagID); err != nil {
			return err
		}
		if err := deleteTagDefinitionTx(tx, preview.SourceTagID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO tag_aliases(alias,tag_id) VALUES(?,?)`, sourceName, preview.TargetTagID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tags SET revision=revision+1 WHERE id=?`, preview.TargetTagID); err != nil {
			return err
		}
		if err := ensureConceptTx(ctx, tx, preview.TargetTagID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tag_concepts SET description=?,revision=revision+1 WHERE tag_id=?`,
			mergedDescription, preview.TargetTagID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO tag_redirects(source_tag_id,target_tag_id,source_name,merged_at,merge_id)
			VALUES(?,?,?,?,?)`, preview.SourceTagID, preview.TargetTagID, sourceName, nowRFC3339(), mergeID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO tag_merge_audit(merge_id,source_tag_id,target_tag_id,
			source_revision,target_revision,preview_json,committed_at) VALUES(?,?,?,?,?,?,?)`,
			mergeID, preview.SourceTagID, preview.TargetTagID, preview.SourceRevision,
			preview.TargetRevision, encoded, nowRFC3339()); err != nil {
			return err
		}
		receipt = TagMergeReceipt{MergeID: mergeID, SourceTagID: preview.SourceTagID, TargetTagID: preview.TargetTagID}
		if audited {
			_, after, err := captureTagMergeSnapshot(ctx, tx)
			if err != nil {
				return err
			}
			return s.persistAuditedTagMerge(ctx, tx, receipt, false, before, after, recordedAt, priorNodes, candidates, scopes)
		}
		return nil
	})
	return receipt, err
}

// ResolveTagRedirect follows a committed identity redirect, including a
// chain of later reviewed merges, without accepting a name as proof of match.
func (s *Store) ResolveTagRedirect(ctx context.Context, tagID string) (string, error) {
	if validateUUIDv4(tagID) != nil {
		return "", ErrNotFound
	}
	seen := map[string]bool{}
	for range 100 {
		if seen[tagID] {
			return "", ErrCycle
		}
		seen[tagID] = true
		var target string
		err := s.db.QueryRowContext(ctx, `SELECT target_tag_id FROM tag_redirects WHERE source_tag_id=?`, tagID).Scan(&target)
		if errors.Is(err, sql.ErrNoRows) {
			_, err = s.TagByID(ctx, tagID)
			return tagID, err
		}
		if err != nil {
			return "", err
		}
		tagID = target
	}
	return "", ErrCycle
}

// ReverseTagMerge restores the original UUID only for the narrow case where
// all moved authority is still distinguishable. A merge touching saved queries,
// passage tags, transferred descriptions, existing target assignments, or
// source aliases needs a separate reviewed migration; guessing would rewrite
// user intent.
func (s *Store) ReverseTagMerge(ctx context.Context, mergeID string) (TagMergeReceipt, error) {
	if validateUUIDv4(mergeID) != nil {
		return TagMergeReceipt{}, ErrNotFound
	}
	var receipt TagMergeReceipt
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var encoded []byte
		var reversedAt sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT preview_json,reversed_at FROM tag_merge_audit WHERE merge_id=?`, mergeID).
			Scan(&encoded, &reversedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if reversedAt.Valid {
			return ErrExists
		}
		var preview TagMergePreview
		if err := json.Unmarshal(encoded, &preview); err != nil {
			return err
		}
		if preview.SourceDescription != "" || len(preview.SavedQueryIDs) != 0 || len(preview.ContentMapIDs) != 0 || len(preview.PassageIDs) != 0 ||
			len(preview.SourceAliases) != 0 || preview.TargetDocumentAssignments != 0 ||
			preview.TargetPassageAssignments != 0 {
			return fmt.Errorf("merge has overlapping or transformed references: %w", ErrInvalidTag)
		}
		audited, err := auditAuthorityActiveTx(ctx, tx)
		if err != nil {
			return err
		}
		recordedAt := nowRFC3339()
		var before []byte
		var priorNodes []Node
		var candidates []auditedTagCandidate
		var scopes []auditScopeState
		if audited {
			_, before, err = captureTagMergeSnapshot(ctx, tx)
			if err != nil {
				return err
			}
			priorNodes, candidates, scopes, err = auditedTagMergeNodesTx(ctx, tx, preview.DocumentNodeIDs)
			if err != nil {
				return err
			}
		}
		target, err := tagByIDTx(tx, preview.TargetTagID)
		if err != nil {
			return err
		}
		concept, err := conceptByTagIDTx(ctx, tx, preview.TargetTagID)
		if err != nil {
			return err
		}
		if target.Revision != preview.TargetRevision+1 || concept.Revision != preview.TargetConceptRev+1 {
			return ErrStaleRevision
		}
		var redirectTarget, aliasTarget string
		if err := tx.QueryRowContext(ctx, `SELECT target_tag_id FROM tag_redirects
			WHERE source_tag_id=? AND merge_id=?`, preview.SourceTagID, mergeID).Scan(&redirectTarget); err != nil {
			return ErrStaleRevision
		}
		if err := tx.QueryRowContext(ctx, `SELECT tag_id FROM tag_aliases WHERE alias=?`, preview.SourceName).Scan(&aliasTarget); err != nil {
			return ErrStaleRevision
		}
		if redirectTarget != preview.TargetTagID || aliasTarget != preview.TargetTagID {
			return ErrStaleRevision
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM tag_aliases WHERE alias=?`, preview.SourceName); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO tags(id,name,revision) VALUES(?,?,?)`,
			preview.SourceTagID, preview.SourceName, preview.SourceRevision+1); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO tag_concepts(tag_id,description,revision) VALUES(?,?,?)`,
			preview.SourceTagID, preview.SourceDescription, preview.SourceConceptRev+1); err != nil {
			return err
		}
		for _, nodeID := range preview.DocumentNodeIDs {
			res, err := tx.ExecContext(ctx, `DELETE FROM node_tags WHERE node_id=? AND tag_id=?`, nodeID, preview.TargetTagID)
			if err != nil {
				return err
			}
			count, err := res.RowsAffected()
			if err != nil || count != 1 {
				return ErrStaleRevision
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO node_tags(node_id,tag_id) VALUES(?,?)`,
				nodeID, preview.SourceTagID); err != nil {
				return err
			}
		}
		if err := touchTaggedNodesTx(tx, preview.SourceTagID, recordedAt); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tags SET revision=revision+1 WHERE id=?`, preview.TargetTagID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tag_concepts SET revision=revision+1 WHERE tag_id=?`, preview.TargetTagID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM tag_redirects WHERE source_tag_id=? AND merge_id=?`, preview.SourceTagID, mergeID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tag_merge_audit SET reversed_at=? WHERE merge_id=?`, nowRFC3339(), mergeID); err != nil {
			return err
		}
		receipt = TagMergeReceipt{MergeID: mergeID, SourceTagID: preview.SourceTagID, TargetTagID: preview.TargetTagID}
		if audited {
			_, after, err := captureTagMergeSnapshot(ctx, tx)
			if err != nil {
				return err
			}
			return s.persistAuditedTagMerge(ctx, tx, receipt, true, before, after, recordedAt, priorNodes, candidates, scopes)
		}
		return nil
	})
	return receipt, err
}
