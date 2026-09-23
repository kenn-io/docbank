package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/docbank/internal/audit"
)

const auditTagMergeTransitionKind = "tag_merge_transition_v1"
const maxAuditedTagMergeSnapshotBytes = 16 << 20

// The transition carries exact logical rows. Concept rows are also covered by
// concept_state_v1; saved queries and maps are checked from these rows because
// older audit lineages did not include them in their attachment genesis.
type tagMergeSnapshot struct {
	Tags        []metadataTag        `json:"tags"`
	Assignments []metadataNodeTag    `json:"assignments"`
	Concepts    []jsontext.Value     `json:"concepts"`
	Passages    []metadataPassageTag `json:"passages"`
	Queries     []metadataSavedQuery `json:"queries"`
	Maps        []jsontext.Value     `json:"maps"`
}

func captureTagMergeSnapshot(ctx context.Context, tx metadataQuerier) (tagMergeSnapshot, []byte, error) {
	state := tagMergeSnapshot{
		Tags: []metadataTag{}, Assignments: []metadataNodeTag{}, Concepts: []jsontext.Value{},
		Passages: []metadataPassageTag{}, Queries: []metadataSavedQuery{}, Maps: []jsontext.Value{},
	}
	write := func(value any) error {
		switch row := value.(type) {
		case metadataTag:
			state.Tags = append(state.Tags, row)
		case metadataNodeTag:
			state.Assignments = append(state.Assignments, row)
		case metadataPassageTag:
			state.Passages = append(state.Passages, row)
		case metadataSavedQuery:
			state.Queries = append(state.Queries, row)
		default:
			encoded, err := json.Marshal(value)
			if err != nil {
				return err
			}
			switch value.(type) {
			case metadataTagConcept, metadataTagAlias, metadataTagConceptEdge, metadataTagRedirect, metadataTagMergeAudit:
				state.Concepts = append(state.Concepts, jsontext.Value(encoded))
			case metadataContentMap, metadataContentMapSnapshot:
				state.Maps = append(state.Maps, jsontext.Value(encoded))
			default:
				return fmt.Errorf("unexpected tag-merge metadata row %T", value)
			}
		}
		return nil
	}
	for _, export := range []func(context.Context, metadataQuerier, metadataWrite) error{
		exportTags, exportNodeTags, exportTagConceptMetadata, exportPassageTagMetadata,
		exportSavedQueries, exportContentMapMetadata,
	} {
		if err := export(ctx, tx, write); err != nil {
			return tagMergeSnapshot{}, nil, err
		}
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return tagMergeSnapshot{}, nil, err
	}
	if len(encoded) > maxAuditedTagMergeSnapshotBytes {
		return tagMergeSnapshot{}, nil, ErrAuditMutationUnsupported
	}
	return state, encoded, nil
}

func decodeTagMergeSnapshot(encoded []byte) (tagMergeSnapshot, error) {
	if len(encoded) == 0 || len(encoded) > maxAuditedTagMergeSnapshotBytes {
		return tagMergeSnapshot{}, errors.New("audited tag merge snapshot has invalid size")
	}
	var state tagMergeSnapshot
	if err := json.Unmarshal(encoded, &state); err != nil {
		return state, err
	}
	canonical, err := json.Marshal(state)
	if err != nil || !bytes.Equal(encoded, canonical) {
		return tagMergeSnapshot{}, errors.New("audited tag merge snapshot is not canonical")
	}
	return state, nil
}

func conceptRecordFromMergeSnapshot(state tagMergeSnapshot) (audit.Record, bool, error) {
	if len(state.Concepts) == 0 && len(state.Passages) == 0 {
		return audit.Record{}, false, nil
	}
	h := sha256.New()
	for _, row := range state.Concepts {
		_, _ = h.Write(append(row, '\n'))
	}
	for _, row := range state.Passages {
		encoded, err := json.Marshal(row)
		if err != nil {
			return audit.Record{}, false, err
		}
		_, _ = h.Write(append(encoded, '\n'))
	}
	digest, err := audit.DigestHex(hex.EncodeToString(h.Sum(nil)))
	if err != nil {
		return audit.Record{}, false, err
	}
	return audit.Record{Kind: auditConceptStateKind, Fields: []audit.Field{{Name: "digest", Value: digest}}}, true, nil
}

func tagMergeTransitionRecord(mergeID string, reverse bool, before, after []byte) (audit.Record, error) {
	id, err := audit.UUID(mergeID)
	if err != nil {
		return audit.Record{}, err
	}
	return audit.Record{Kind: auditTagMergeTransitionKind, Fields: []audit.Field{
		{Name: "merge_id", Value: id},
		{Name: "reverse", Value: audit.Bool(reverse)},
		{Name: "before", Value: audit.Bytes(before)},
		{Name: "after", Value: audit.Bytes(after)},
	}}, nil
}

func (s *Store) persistAuditedTagMerge(
	ctx context.Context, tx *sql.Tx, receipt TagMergeReceipt, reverse bool,
	before, after []byte, recordedAt string, priorNodes []Node, candidates []auditedTagCandidate, scopes []auditScopeState,
) error {
	authority, nodeSequence, err := loadAuditAuthorityTx(ctx, tx)
	if err != nil {
		return err
	}
	operationID, err := newUUIDv4()
	if err != nil {
		return err
	}
	values, err := makeAuditedMutationValues(s.vaultID, authority.lineageID, operationID, recordedAt)
	if err != nil {
		return err
	}
	transition, err := tagMergeTransitionRecord(receipt.MergeID, reverse, before, after)
	if err != nil {
		return err
	}
	change, err := makeAttachedMetadataPresenceChange(transition, true)
	if err != nil {
		return err
	}
	delta, digest, err := makeAttachedMetadataDelta(values.operationID, []audit.Record{change})
	if err != nil {
		return err
	}
	sequence, err := nextAuditInteger("operation sequence", authority.sequence)
	if err != nil {
		return err
	}
	if err := insertAuditRecord(ctx, tx, delta); err != nil {
		return err
	}
	mutationHash := audit.Absent()
	if len(candidates) != 0 {
		priorByID := nodesByID(priorNodes)
		resultingByID := make(map[int64]Node, len(priorNodes))
		stateChanges := make([]audit.Record, len(priorNodes))
		for i, prior := range priorNodes {
			resultingByID[prior.ID], err = nodeByIDTx(tx, prior.ID)
			if err != nil {
				return err
			}
			stateChanges[i], err = makeAuditMemberStateChange(prior, resultingByID[prior.ID])
			if err != nil {
				return err
			}
		}
		events := make([]audit.Record, len(candidates))
		beforeState, err := decodeTagMergeSnapshot(before)
		if err != nil {
			return err
		}
		if reverse {
			beforeState, err = decodeTagMergeSnapshot(after)
		}
		if err != nil {
			return err
		}
		source, err := tagMergeSourceDefinition(beforeState, receipt.SourceTagID)
		if err != nil {
			return err
		}
		for i, candidate := range candidates {
			if reverse {
				assignment, assignmentErr := auditTagAssignmentRecord(receipt.SourceTagID, candidate.nodeID)
				if assignmentErr != nil {
					return assignmentErr
				}
				events[i], err = makeAuditedTagAssignmentEvent(values, candidate.scopeID, uint64(i),
					priorByID[candidate.nodeID], resultingByID[candidate.nodeID], assignment, true)
			} else {
				events[i], err = makeAuditedTagDeleteEvent(values, candidate.scopeID, uint64(i),
					priorByID[candidate.nodeID], resultingByID[candidate.nodeID], source)
			}
			if err != nil {
				return err
			}
			kind := "tag_merge"
			if reverse {
				kind = "tag_merge_reverse"
			}
			kindValue, err := audit.Text(kind)
			if err != nil {
				return err
			}
			events[i], err = replaceAuditRecordField(events[i], "event_kind", kindValue)
			if err != nil {
				return err
			}
		}
		mutation, err := makeAuditedMemberStatesMutation(values, sequence, events, stateChanges)
		if err != nil {
			return err
		}
		mutation, err = replaceAuditRecordField(mutation, auditAttachedMetadataChangeCountField, audit.Unsigned(1))
		if err != nil {
			return err
		}
		mutation, err = replaceAuditRecordField(mutation, "attached_metadata_change_digest", digest.value)
		if err != nil {
			return err
		}
		hash, err := hashAuditRecord(mutation)
		if err != nil {
			return err
		}
		for _, event := range events {
			if err := insertAuditRecord(ctx, tx, audit.Record{Kind: auditEventField, Fields: []audit.Field{
				{Name: auditEventField, Value: audit.Nested(event)},
			}}); err != nil {
				return err
			}
		}
		if err := insertAuditRecord(ctx, tx, mutation); err != nil {
			return err
		}
		if err := advanceAuditedMutationScopes(ctx, tx, values, scopes, hash.value); err != nil {
			return err
		}
		mutationHash = hash.value
	}
	allocation, err := makeAuditAllocationEntry(values, sequence, nodeSequence, authority.allocationHead, mutationHash)
	if err != nil {
		return err
	}
	allocation, err = addAttachedMetadataToAllocation(allocation, 1, digest.value)
	if err != nil {
		return err
	}
	return advanceAuditAuthority(ctx, tx, authority, sequence, allocation)
}

func tagMergeSourceDefinition(state tagMergeSnapshot, id string) (audit.Record, error) {
	for _, row := range state.Tags {
		if row.ID == id {
			return tagDefinitionAuditRecord(Tag{ID: id, Name: row.Name})
		}
	}
	return audit.Record{}, ErrNotFound
}

func auditedTagMergeNodesTx(ctx context.Context, tx *sql.Tx, nodeIDs []int64) ([]Node, []auditedTagCandidate, []auditScopeState, error) {
	var nodes []Node
	var candidates []auditedTagCandidate
	byScope := map[string]auditScopeState{}
	for _, nodeID := range nodeIDs {
		foundScopes, err := auditedTagMergeNodeScopesTx(ctx, tx, nodeID)
		if err != nil {
			return nil, nil, nil, err
		}
		for _, scope := range foundScopes {
			candidates = append(candidates, auditedTagCandidate{nodeID: nodeID, scopeID: scope.scopeID})
			byScope[scope.scopeID] = scope
		}
		if len(foundScopes) != 0 {
			node, err := nodeByIDTx(tx, nodeID)
			if err != nil {
				return nil, nil, nil, err
			}
			nodes = append(nodes, node)
		}
	}
	scopes := make([]auditScopeState, 0, len(byScope))
	for _, scope := range byScope {
		scopes = append(scopes, scope)
	}
	slices.SortFunc(scopes, func(a, b auditScopeState) int {
		if a.scopeID < b.scopeID {
			return -1
		}
		if a.scopeID > b.scopeID {
			return 1
		}
		return 0
	})
	return nodes, candidates, scopes, nil
}

func auditedTagMergeNodeScopesTx(ctx context.Context, tx *sql.Tx, nodeID int64) ([]auditScopeState, error) {
	rows, err := tx.QueryContext(ctx, `SELECT scope.scope_id,scope.entry_count,scope.chain_head
		FROM audit_memberships member JOIN audit_scopes scope ON scope.scope_id=member.scope_id
		WHERE member.node_id=? ORDER BY scope.scope_id`, nodeID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []auditScopeState
	for rows.Next() {
		var scope auditScopeState
		if err := rows.Scan(&scope.scopeID, &scope.entryCount, &scope.chainHead); err != nil {
			return nil, err
		}
		result = append(result, scope)
	}
	return result, rows.Err()
}
