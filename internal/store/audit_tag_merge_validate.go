package store

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"go.kenn.io/docbank/internal/audit"
	"go.kenn.io/docbank/internal/query"
)

type replayedTagMergeTransition struct {
	mergeID string
	reverse bool
	before  tagMergeSnapshot
	after   tagMergeSnapshot
	digest  string
	preview TagMergePreview
}

func equalTagMergeQueryMapRows(a, b tagMergeSnapshot) bool {
	return reflect.DeepEqual(a.Queries, b.Queries) && reflect.DeepEqual(a.Maps, b.Maps)
}

func (replay *auditedHistoryReplay) readTagMergeTransition(
	operationID, digest string, deltas map[string]storedAuditRecord, used map[string]bool,
) (replayedTagMergeTransition, bool, error) {
	delta, ok := deltas[digest]
	if !ok || used[digest] {
		return replayedTagMergeTransition{}, false, errors.New("audited tag merge lacks a unique delta")
	}
	if err := requireAuditUUID(delta.record, auditOperationIDField, operationID); err != nil {
		return replayedTagMergeTransition{}, false, err
	}
	changes, err := auditRecordListField(delta.record, "changes")
	if err != nil {
		return replayedTagMergeTransition{}, false, err
	}
	if len(changes) != 1 {
		return replayedTagMergeTransition{}, false, nil
	}
	change := changes[0]
	kind, err := auditTextField(change, "record_kind")
	if err != nil || kind != auditTagMergeTransitionKind {
		return replayedTagMergeTransition{}, false, err
	}
	if err := requireAuditAbsent(change, auditPreField); err != nil {
		return replayedTagMergeTransition{}, true, err
	}
	marker, err := auditNestedField(change, auditPostField)
	if err != nil || marker.Kind != auditTagMergeTransitionKind {
		return replayedTagMergeTransition{}, true, errors.New("audited tag merge has invalid marker")
	}
	identity, err := attachedAuditIdentity(marker)
	if err != nil {
		return replayedTagMergeTransition{}, true, err
	}
	storedIdentity, err := auditNestedField(change, "stable_identity")
	if err != nil || !auditRecordEqual(identity, storedIdentity) {
		return replayedTagMergeTransition{}, true, errors.New("audited tag merge identity mismatch")
	}
	mergeID, err := auditUUIDField(marker, "merge_id")
	if err != nil {
		return replayedTagMergeTransition{}, true, err
	}
	reverse, err := auditBoolField(marker, "reverse")
	if err != nil {
		return replayedTagMergeTransition{}, true, err
	}
	preValue, err := auditField(marker, "before")
	if err != nil {
		return replayedTagMergeTransition{}, true, err
	}
	beforeBytes, ok := preValue.BytesValue()
	if !ok {
		return replayedTagMergeTransition{}, true, errors.New("merge before is not bytes")
	}
	postValue, err := auditField(marker, "after")
	if err != nil {
		return replayedTagMergeTransition{}, true, err
	}
	afterBytes, ok := postValue.BytesValue()
	if !ok {
		return replayedTagMergeTransition{}, true, errors.New("merge after is not bytes")
	}
	before, err := decodeTagMergeSnapshot(beforeBytes)
	if err != nil {
		return replayedTagMergeTransition{}, true, err
	}
	after, err := decodeTagMergeSnapshot(afterBytes)
	if err != nil {
		return replayedTagMergeTransition{}, true, err
	}
	transition := replayedTagMergeTransition{mergeID: mergeID, reverse: reverse, before: before, after: after, digest: digest}
	if err := transition.validateShape(); err != nil {
		return replayedTagMergeTransition{}, true, err
	}
	if err := replay.checkMergeSnapshotBefore(before); err != nil {
		return replayedTagMergeTransition{}, true, err
	}
	if replay.lastTagMergeRows != nil && !equalTagMergeQueryMapRows(*replay.lastTagMergeRows, before) {
		return replayedTagMergeTransition{}, true, errors.New("audited tag merge query/map pre-state changed outside replay")
	}
	used[digest] = true
	return transition, true, nil
}

func (transition *replayedTagMergeTransition) validateShape() error {
	var beforeAudit, afterAudit *metadataTagMergeAudit
	for _, item := range transition.before.Concepts {
		var header struct {
			Type    string `json:"type"`
			MergeID string `json:"merge_id"`
		}
		if err := json.Unmarshal(item, &header); err != nil {
			return err
		}
		if header.Type == metadataTagMergeAuditType && header.MergeID == transition.mergeID {
			var row metadataTagMergeAudit
			if err := json.Unmarshal(item, &row); err != nil {
				return err
			}
			beforeAudit = &row
		}
	}
	for _, item := range transition.after.Concepts {
		var header struct {
			Type    string `json:"type"`
			MergeID string `json:"merge_id"`
		}
		if err := json.Unmarshal(item, &header); err != nil {
			return err
		}
		if header.Type == metadataTagMergeAuditType && header.MergeID == transition.mergeID {
			var row metadataTagMergeAudit
			if err := json.Unmarshal(item, &row); err != nil {
				return err
			}
			afterAudit = &row
		}
	}
	if afterAudit == nil {
		return errors.New("audited tag merge lacks its merge receipt")
	}
	if transition.reverse {
		if beforeAudit == nil || beforeAudit.ReversedAt != nil || afterAudit.ReversedAt == nil ||
			beforeAudit.SourceTagID != afterAudit.SourceTagID || beforeAudit.TargetTagID != afterAudit.TargetTagID ||
			!bytes.Equal(beforeAudit.PreviewJSON, afterAudit.PreviewJSON) {
			return errors.New("audited tag reverse has an invalid receipt transition")
		}
	} else if beforeAudit != nil || afterAudit.ReversedAt != nil {
		return errors.New("audited tag merge repeats or reverses its receipt")
	}
	if err := json.Unmarshal(afterAudit.PreviewJSON, &transition.preview); err != nil {
		return err
	}
	if transition.preview.SourceTagID != afterAudit.SourceTagID || transition.preview.TargetTagID != afterAudit.TargetTagID ||
		transition.preview.SourceTagID == transition.preview.TargetTagID {
		return errors.New("audited tag merge receipt does not match its preview")
	}
	encodedPreview, err := json.Marshal(transition.preview)
	if err != nil {
		return err
	}
	if !bytes.Equal(encodedPreview, afterAudit.PreviewJSON) ||
		afterAudit.SourceRevision != transition.preview.SourceRevision ||
		afterAudit.TargetRevision != transition.preview.TargetRevision {
		return errors.New("audited tag merge receipt changes its reviewed preview")
	}
	if err := validateMergeTagAndAssignmentRows(transition); err != nil {
		return err
	}
	if err := validateMergeOtherRows(transition); err != nil {
		return err
	}
	if err := validateMergeAliasRedirectPassageRows(transition); err != nil {
		return err
	}
	if err := validateMergeQueryRows(transition); err != nil {
		return err
	}
	return validateMergeMapRows(transition)
}

func validateMergeAliasRedirectPassageRows(t *replayedTagMergeTransition) error {
	aliases := func(rows []jsontext.Value) (map[string]string, error) {
		result := map[string]string{}
		for _, raw := range rows {
			var header struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(raw, &header); err != nil {
				return nil, err
			}
			if header.Type != metadataTagAliasType {
				continue
			}
			var row metadataTagAlias
			if err := json.Unmarshal(raw, &row); err != nil {
				return nil, err
			}
			if _, ok := result[row.Alias]; ok {
				return nil, errors.New("duplicate alias in merge snapshot")
			}
			result[row.Alias] = row.TagID
		}
		return result, nil
	}
	priorAliases, err := aliases(t.before.Concepts)
	if err != nil {
		return err
	}
	resultingAliases, err := aliases(t.after.Concepts)
	if err != nil {
		return err
	}
	wantAliases := make(map[string]string, len(priorAliases)+1)
	maps.Copy(wantAliases, priorAliases)
	if t.reverse {
		if wantAliases[t.preview.SourceName] != t.preview.TargetTagID {
			return errors.New("reverse lacks source-name alias")
		}
		delete(wantAliases, t.preview.SourceName)
	} else {
		for _, name := range t.preview.SourceAliases {
			if wantAliases[name] != t.preview.SourceTagID {
				return errors.New("merge source alias pre-state is invalid")
			}
			wantAliases[name] = t.preview.TargetTagID
		}
		wantAliases[t.preview.SourceName] = t.preview.TargetTagID
	}
	if !reflect.DeepEqual(wantAliases, resultingAliases) {
		return errors.New("audited tag merge alias migration is invalid")
	}

	redirects := func(rows []jsontext.Value) (map[string]metadataTagRedirect, error) {
		result := map[string]metadataTagRedirect{}
		for _, raw := range rows {
			var header struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(raw, &header); err != nil {
				return nil, err
			}
			if header.Type != metadataTagRedirectType {
				continue
			}
			var row metadataTagRedirect
			if err := json.Unmarshal(raw, &row); err != nil {
				return nil, err
			}
			result[row.SourceTagID] = row
		}
		return result, nil
	}
	priorRedirects, err := redirects(t.before.Concepts)
	if err != nil {
		return err
	}
	resultingRedirects, err := redirects(t.after.Concepts)
	if err != nil {
		return err
	}
	if t.reverse {
		row, ok := priorRedirects[t.preview.SourceTagID]
		if !ok || row.MergeID != t.mergeID || row.TargetTagID != t.preview.TargetTagID {
			return errors.New("reverse lacks its source redirect")
		}
		delete(priorRedirects, t.preview.SourceTagID)
	} else {
		for id, row := range priorRedirects {
			if row.TargetTagID == t.preview.SourceTagID {
				row.TargetTagID = t.preview.TargetTagID
				priorRedirects[id] = row
			}
		}
		row, ok := resultingRedirects[t.preview.SourceTagID]
		if !ok || row.TargetTagID != t.preview.TargetTagID || row.SourceName != t.preview.SourceName || row.MergeID != t.mergeID {
			return errors.New("merge lacks its exact source redirect")
		}
		priorRedirects[t.preview.SourceTagID] = row
	}
	if !reflect.DeepEqual(priorRedirects, resultingRedirects) {
		return errors.New("audited tag merge redirects differ from reviewed migration")
	}

	wantPassages := map[string]metadataPassageTag{}
	for _, row := range t.before.Passages {
		wantPassages[row.PassageID+"/"+row.TagID] = row
	}
	if t.reverse {
		if len(t.preview.PassageIDs) != 0 {
			return errors.New("reverse of passage-tag merge is unsupported")
		}
	} else {
		for _, passageID := range t.preview.PassageIDs {
			key := passageID + "/" + t.preview.SourceTagID
			row, ok := wantPassages[key]
			if !ok {
				return errors.New("merge omits previewed passage assignment")
			}
			delete(wantPassages, key)
			row.TagID = t.preview.TargetTagID
			targetKey := passageID + "/" + t.preview.TargetTagID
			if _, exists := wantPassages[targetKey]; !exists {
				wantPassages[targetKey] = row
			}
		}
	}
	actualPassages := map[string]metadataPassageTag{}
	for _, row := range t.after.Passages {
		actualPassages[row.PassageID+"/"+row.TagID] = row
	}
	if !reflect.DeepEqual(wantPassages, actualPassages) {
		return errors.New("audited tag merge passage migration is invalid")
	}
	return nil
}

func validateMergeTagAndAssignmentRows(t *replayedTagMergeTransition) error {
	before := map[string]metadataTag{}
	after := map[string]metadataTag{}
	for _, row := range t.before.Tags {
		before[row.ID] = row
	}
	for _, row := range t.after.Tags {
		after[row.ID] = row
	}
	source, sourceBefore := before[t.preview.SourceTagID]
	target, targetBefore := before[t.preview.TargetTagID]
	if !targetBefore || after[t.preview.TargetTagID].Revision != target.Revision+1 || after[t.preview.TargetTagID].Name != target.Name {
		return errors.New("audited tag merge target definition transition is invalid")
	}
	if t.reverse {
		if sourceBefore || after[t.preview.SourceTagID].ID != t.preview.SourceTagID ||
			after[t.preview.SourceTagID].Name != t.preview.SourceName ||
			after[t.preview.SourceTagID].Revision != t.preview.SourceRevision+1 {
			return errors.New("audited tag reverse does not restore the source identity")
		}
	} else if !sourceBefore || source.Name != t.preview.SourceName || after[t.preview.SourceTagID].ID != "" {
		return errors.New("audited tag merge does not retire the source identity")
	}
	if t.preview.DocumentAssignments != len(t.preview.DocumentNodeIDs) ||
		!slices.IsSorted(t.preview.DocumentNodeIDs) ||
		len(slices.Compact(slices.Clone(t.preview.DocumentNodeIDs))) != len(t.preview.DocumentNodeIDs) {
		return errors.New("audited tag merge preview has invalid assignment set")
	}
	for id, row := range before {
		if id != t.preview.SourceTagID && id != t.preview.TargetTagID && !reflect.DeepEqual(row, after[id]) {
			return fmt.Errorf("audited tag merge changes unrelated tag %s", id)
		}
	}
	for id := range after {
		if id != t.preview.SourceTagID && id != t.preview.TargetTagID {
			if _, ok := before[id]; !ok {
				return fmt.Errorf("audited tag merge creates unrelated tag %s", id)
			}
		}
	}
	assignmentSet := func(rows []metadataNodeTag) map[string]bool {
		set := make(map[string]bool, len(rows))
		for _, row := range rows {
			set[fmt.Sprintf("%d/%s", row.NodeID, row.TagID)] = true
		}
		return set
	}
	want := assignmentSet(t.before.Assignments)
	for _, nodeID := range t.preview.DocumentNodeIDs {
		sourceKey := fmt.Sprintf("%d/%s", nodeID, t.preview.SourceTagID)
		targetKey := fmt.Sprintf("%d/%s", nodeID, t.preview.TargetTagID)
		if t.reverse {
			if !want[targetKey] || want[sourceKey] {
				return errors.New("audited tag reverse has invalid prior assignment")
			}
			delete(want, targetKey)
			want[sourceKey] = true
		} else {
			if !want[sourceKey] {
				return errors.New("audited tag merge omits prior source assignment")
			}
			delete(want, sourceKey)
			want[targetKey] = true
		}
	}
	if !reflect.DeepEqual(want, assignmentSet(t.after.Assignments)) {
		return errors.New("audited tag merge assignments do not match its preview")
	}
	return nil
}

func validateMergeQueryRows(t *replayedTagMergeTransition) error {
	before := make(map[string]metadataSavedQuery, len(t.before.Queries))
	after := make(map[string]metadataSavedQuery, len(t.after.Queries))
	for _, row := range t.before.Queries {
		before[row.ID] = row
	}
	for _, row := range t.after.Queries {
		after[row.ID] = row
	}
	if t.reverse && len(t.preview.SavedQueryIDs) != 0 {
		return errors.New("audited reverse rewrites a saved query")
	}
	for _, id := range t.preview.SavedQueryIDs {
		prior, ok := before[id]
		if !ok {
			return errors.New("audited tag merge lacks previewed saved query")
		}
		result, ok := after[id]
		if !ok || result.Revision != prior.Revision+1 || result.Name != prior.Name ||
			result.Description != prior.Description || result.Kind != prior.Kind || result.CreatedAt != prior.CreatedAt {
			return errors.New("audited tag merge has invalid saved query revision")
		}
		parsed, err := query.Parse(prior.Payload)
		if err != nil {
			return err
		}
		for _, values := range [][]string{parsed.Filters.TagIDs, parsed.Filters.ExcludeTagIDs} {
			for i := range values {
				if values[i] == t.preview.SourceTagID {
					values[i] = t.preview.TargetTagID
				}
			}
		}
		canonical, fingerprint, err := query.CanonicalWithFingerprint(parsed)
		if err != nil {
			return err
		}
		if !bytes.Equal(result.Payload, canonical) || result.Fingerprint != fingerprint {
			return errors.New("audited tag merge saved query rewrite differs from preview")
		}
	}
	return nil
}

func validateMergeMapRows(t *replayedTagMergeTransition) error {
	before := map[string]metadataContentMap{}
	after := map[string]metadataContentMap{}
	for _, raw := range t.before.Maps {
		var header struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &header); err != nil {
			return err
		}
		if header.Type == metadataContentMapType {
			var row metadataContentMap
			if err := json.Unmarshal(raw, &row); err != nil {
				return err
			}
			before[row.ID] = row
		}
	}
	for _, raw := range t.after.Maps {
		var header struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &header); err != nil {
			return err
		}
		if header.Type == metadataContentMapType {
			var row metadataContentMap
			if err := json.Unmarshal(raw, &row); err != nil {
				return err
			}
			after[row.ID] = row
		}
	}
	if t.reverse && len(t.preview.ContentMapIDs) != 0 {
		return errors.New("audited reverse rewrites a content map")
	}
	for _, id := range t.preview.ContentMapIDs {
		prior, ok := before[id]
		if !ok {
			return errors.New("audited tag merge lacks previewed content map")
		}
		result, ok := after[id]
		if !ok || result.Revision != prior.Revision+1 || result.Owner != prior.Owner ||
			result.CreatedAt != prior.CreatedAt || !reflect.DeepEqual(result.ArchivedAt, prior.ArchivedAt) {
			return errors.New("audited tag merge has invalid content map revision")
		}
		var definition ContentMapDefinition
		if err := json.Unmarshal(prior.DefinitionJSON, &definition); err != nil {
			return err
		}
		for i := range definition.Sections {
			selector := definition.Sections[i].Selector
			if selector == nil {
				continue
			}
			for _, values := range [][]string{selector.Filters.TagIDs, selector.Filters.ExcludeTagIDs} {
				for j := range values {
					if values[j] == t.preview.SourceTagID {
						values[j] = t.preview.TargetTagID
					}
				}
			}
		}
		plan, canonical, err := normalizedMapDefinition(definition)
		if err != nil {
			return err
		}
		if !bytes.Equal(result.DefinitionJSON, canonical) || result.DefinitionDigest != plan.DefinitionDigest {
			return errors.New("audited tag merge content map rewrite differs from preview")
		}
	}
	return nil
}

func validateMergeOtherRows(t *replayedTagMergeTransition) error {
	// Exact before/after rows are authenticated by the audit lineage. Limit the
	// mutable keys to the previewed source/target and query/map references.
	for _, group := range []struct {
		before, after [][]byte
		allowed       func(string, string) bool
	}{
		{mergeJSONRows(t.before.Concepts), mergeJSONRows(t.after.Concepts), func(kind, key string) bool {
			switch kind {
			case metadataTagConceptType:
				return key == t.preview.SourceTagID || key == t.preview.TargetTagID
			case metadataTagAliasType:
				return key == t.preview.SourceName || slices.Contains(t.preview.SourceAliases, key)
			case metadataTagConceptEdgeType:
				return false
			case metadataTagRedirectType:
				return key == t.preview.SourceTagID || key != ""
			case metadataTagMergeAuditType:
				return key == t.mergeID
			}
			return false
		}},
		{mergePassageRows(t.before.Passages), mergePassageRows(t.after.Passages), func(_, key string) bool {
			for _, id := range t.preview.PassageIDs {
				if strings.HasPrefix(key, id+"/") {
					return true
				}
			}
			return false
		}},
		{mergeQueryRows(t.before.Queries), mergeQueryRows(t.after.Queries), func(_, key string) bool { return slices.Contains(t.preview.SavedQueryIDs, key) }},
		{mergeJSONRows(t.before.Maps), mergeJSONRows(t.after.Maps), func(kind, key string) bool {
			return kind == metadataContentMapType && slices.Contains(t.preview.ContentMapIDs, key)
		}},
	} {
		before, err := mergeRowsByKey(group.before)
		if err != nil {
			return err
		}
		after, err := mergeRowsByKey(group.after)
		if err != nil {
			return err
		}
		for key, old := range before {
			if next, ok := after[key]; !ok || !bytes.Equal(old, next) {
				parts := strings.SplitN(key, "\x00", 2)
				if !group.allowed(parts[0], parts[1]) {
					return fmt.Errorf("audited tag merge changes unpreviewed %s row %s", parts[0], parts[1])
				}
			}
		}
		for key := range after {
			if _, ok := before[key]; !ok {
				parts := strings.SplitN(key, "\x00", 2)
				if !group.allowed(parts[0], parts[1]) {
					return fmt.Errorf("audited tag merge adds unpreviewed %s row %s", parts[0], parts[1])
				}
			}
		}
	}
	return nil
}

func mergeJSONRows[T ~[]byte](rows []T) [][]byte {
	result := make([][]byte, len(rows))
	for i, row := range rows {
		result[i] = row
	}
	return result
}

func mergePassageRows(rows []metadataPassageTag) [][]byte { return marshalMergeRows(rows) }
func mergeQueryRows(rows []metadataSavedQuery) [][]byte   { return marshalMergeRows(rows) }
func marshalMergeRows[T any](rows []T) [][]byte {
	result := make([][]byte, len(rows))
	for i, row := range rows {
		result[i], _ = json.Marshal(row)
	}
	return result
}

func mergeRowsByKey(rows [][]byte) (map[string][]byte, error) {
	result := make(map[string][]byte, len(rows))
	for _, row := range rows {
		var fields map[string]jsontext.Value
		if err := json.Unmarshal(row, &fields); err != nil {
			return nil, err
		}
		var typ string
		if err := json.Unmarshal(fields["type"], &typ); err != nil {
			return nil, err
		}
		keyField := "id"
		switch typ {
		case metadataTagConceptType:
			keyField = "tag_id"
		case metadataTagAliasType:
			keyField = "alias"
		case metadataTagConceptEdgeType:
			keyField = "parent_tag_id"
		case metadataTagRedirectType:
			keyField = "source_tag_id"
		case metadataTagMergeAuditType:
			keyField = "merge_id"
		case metadataPassageTagType:
			keyField = "passage_id"
		case metadataSavedQueryType:
			keyField = "saved_query_id"
		}
		var id string
		if err := json.Unmarshal(fields[keyField], &id); err != nil {
			return nil, err
		}
		if typ == metadataPassageTagType {
			var tagID string
			if err := json.Unmarshal(fields["tag_id"], &tagID); err != nil {
				return nil, err
			}
			id += "/" + tagID
		}
		if typ == metadataTagConceptEdgeType {
			var child, kind string
			if err := json.Unmarshal(fields["child_tag_id"], &child); err != nil {
				return nil, err
			}
			if err := json.Unmarshal(fields["kind"], &kind); err != nil {
				return nil, err
			}
			id += "/" + child + "/" + kind
		}
		key := typ + "\x00" + id
		if _, exists := result[key]; exists {
			return nil, fmt.Errorf("audited tag merge repeats %s", key)
		}
		result[key] = row
	}
	return result, nil
}

func mergeSnapshotAttachments(state tagMergeSnapshot) (map[string]audit.Record, error) {
	result := map[string]audit.Record{}
	add := func(record audit.Record) error {
		key, err := attachedAuditKey(record)
		if err != nil {
			return err
		}
		if _, exists := result[key]; exists {
			return errors.New("audited tag merge snapshot repeats attachment")
		}
		result[key] = record
		return nil
	}
	for _, tag := range state.Tags {
		record, err := tagDefinitionAuditRecord(Tag{ID: tag.ID, Name: tag.Name})
		if err != nil {
			return nil, err
		}
		if err := add(record); err != nil {
			return nil, err
		}
	}
	for _, assignment := range state.Assignments {
		record, err := auditTagAssignmentRecord(assignment.TagID, assignment.NodeID)
		if err != nil {
			return nil, err
		}
		if err := add(record); err != nil {
			return nil, err
		}
	}
	concept, present, err := conceptRecordFromMergeSnapshot(state)
	if err != nil {
		return nil, err
	}
	if present {
		if err := add(concept); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func isMergeAttachmentKind(kind string) bool {
	return kind == auditTagDefinitionKind || kind == auditTagAssignmentKind || kind == auditConceptStateKind
}

func (replay *auditedHistoryReplay) checkMergeSnapshotBefore(state tagMergeSnapshot) error {
	expected, err := mergeSnapshotAttachments(state)
	if err != nil {
		return err
	}
	for key, record := range expected {
		current, exists := replay.attachments[key]
		if !exists || !auditRecordEqual(record, current) {
			return errors.New("audited tag merge pre-state differs from replayed attachments")
		}
	}
	for key, record := range replay.attachments {
		if isMergeAttachmentKind(record.Kind) {
			if _, exists := expected[key]; !exists {
				return errors.New("audited tag merge pre-state omits replayed attachment")
			}
		}
	}
	return nil
}

func (replay *auditedHistoryReplay) applyTagMergeSnapshotAfter(state tagMergeSnapshot) error {
	resulting, err := mergeSnapshotAttachments(state)
	if err != nil {
		return err
	}
	for key, record := range replay.attachments {
		if isMergeAttachmentKind(record.Kind) {
			delete(replay.attachments, key)
		}
	}
	for key, record := range resulting {
		replay.attachments[key] = record
		if record.Kind == auditTagDefinitionKind {
			id, err := auditUUIDField(record, "tag_id")
			if err != nil {
				return err
			}
			replay.tagDefinitionIDs[id] = true
		}
	}
	snapshotCopy := state
	replay.lastTagMergeRows = &snapshotCopy
	return nil
}

func (replay *auditedHistoryReplay) tagMergeCandidates(preview TagMergePreview) ([]replayedAuditedTagCandidate, error) {
	var result []replayedAuditedTagCandidate
	for _, nodeID := range preview.DocumentNodeIDs {
		auditNodeID, err := positiveAuditNodeID(nodeID)
		if err != nil {
			return nil, err
		}
		for scopeID, scope := range replay.scopes {
			if scope.memberSet[auditNodeID] {
				result = append(result, replayedAuditedTagCandidate{nodeID: auditNodeID, scopeID: scopeID})
			}
		}
	}
	slices.SortFunc(result, func(a, b replayedAuditedTagCandidate) int {
		if a.nodeID < b.nodeID {
			return -1
		}
		if a.nodeID > b.nodeID {
			return 1
		}
		return strings.Compare(a.scopeID, b.scopeID)
	})
	return result, nil
}

func (replay *auditedHistoryReplay) applyUnscopedTagMergeTransition(
	operationID, digest string, allocation storedAuditRecord, nextCount int64,
	deltas map[string]storedAuditRecord, used map[string]bool,
) (bool, error) {
	transition, handled, err := replay.readTagMergeTransition(operationID, digest, deltas, used)
	if !handled || err != nil {
		return handled, err
	}
	candidates, err := replay.tagMergeCandidates(transition.preview)
	if err != nil {
		return true, err
	}
	if len(candidates) != 0 {
		return true, errors.New("unscoped tag merge omits audited node effects")
	}
	if err := requireAuditUnsigned(allocation.record, auditAttachedMetadataChangeCountField, 1); err != nil {
		return true, err
	}
	if err := replay.applyTagMergeSnapshotAfter(transition.after); err != nil {
		return true, err
	}
	replay.allocationCount, replay.allocationHead = nextCount, allocation.digest
	return true, nil
}

func (replay *auditedHistoryReplay) applyScopedTagMergeTransition(
	vaultID string, mutation, allocation storedAuditRecord,
	scopeIDs []string, scopeEntries map[string]storedAuditRecord,
	deltas, events map[string]storedAuditRecord, usedDeltas, usedEvents map[string]bool,
) error {
	operationID, err := auditUUIDField(mutation.record, auditOperationIDField)
	if err != nil {
		return err
	}
	digest, err := auditDigestField(mutation.record, "attached_metadata_change_digest")
	if err != nil {
		return err
	}
	transition, handled, err := replay.readTagMergeTransition(operationID, digest, deltas, usedDeltas)
	if err != nil {
		return err
	}
	if !handled {
		return errors.New("tag merge mutation lacks its transition")
	}
	candidates, err := replay.tagMergeCandidates(transition.preview)
	if err != nil {
		return err
	}
	if len(candidates) == 0 {
		return errors.New("scoped tag merge has no audited node effects")
	}
	if err := requireAuditedTagScopeFanout("merge", scopeIDs, candidates); err != nil {
		return err
	}
	if err := requireAuditUUID(mutation.record, auditVaultIDField, vaultID); err != nil {
		return err
	}
	sequence, err := positiveAuditInteger("operation sequence", replay.allocationCount+1)
	if err != nil {
		return err
	}
	if err := requireAuditUnsigned(mutation.record, "operation_sequence", sequence); err != nil {
		return err
	}
	if err := requireAuditAbsent(mutation.record, "grouping_id"); err != nil {
		return err
	}
	if err := requireAuditUnsigned(mutation.record, auditAttachedMetadataChangeCountField, 1); err != nil {
		return err
	}
	if err := requireNoChangeMutationFieldsExceptAttachment(mutation.record); err != nil {
		return err
	}
	wrappedEvents, err := auditRecordListField(mutation.record, "events")
	if err != nil {
		return err
	}
	if len(wrappedEvents) != len(candidates) {
		return errors.New("tag merge omits audited member events")
	}
	for i, candidate := range candidates {
		event := wrappedEvents[i]
		if err := validateAuditEventWrapper(operationID, uint64(i), event, events, usedEvents); err != nil {
			return err
		}
		kind := "tag_merge"
		if transition.reverse {
			kind = "tag_merge_reverse"
		}
		state := replay.states[candidate.nodeID]
		revision, err := auditUnsignedField(state, "node_revision")
		if err != nil {
			return err
		}
		version, err := auditOptionalUUIDField(state, "current_version_id")
		if err != nil {
			return err
		}
		for _, check := range []func() error{
			func() error { return requireAuditUUID(event, auditOperationIDField, operationID) },
			func() error { return requireAuditUnsigned(event, metadataNodeIDField, candidate.nodeID) },
			func() error { return requireAuditUUID(event, auditScopeIDField, candidate.scopeID) },
			func() error { return requireAuditText(event, "event_kind", kind) },
			func() error { return requireAuditUnsigned(event, "prior_node_revision", revision) },
			func() error { return requireAuditUnsigned(event, "resulting_node_revision", revision+1) },
			func() error { return requireAuditOptionalUUID(event, "prior_current_version_id", version) },
			func() error { return requireAuditOptionalUUID(event, "resulting_current_version_id", version) },
			func() error { return requireMatchingEventEnvelope(mutation.record, event) },
			func() error {
				return requireAuditAbsentFields(event, auditTargetNodeIDField, "source_version_id", auditTopologyDeltaField, "baseline_digest")
			},
		} {
			if err := check(); err != nil {
				return err
			}
		}
		if transition.reverse {
			tagID, err := audit.UUID(transition.preview.SourceTagID)
			if err != nil {
				return err
			}
			assignment := audit.Record{Kind: auditTagAssignmentKind, Fields: []audit.Field{
				{Name: "tag_id", Value: tagID}, {Name: metadataNodeIDField, Value: audit.Unsigned(candidate.nodeID)},
			}}
			if err != nil {
				return err
			}
			identity, err := attachedAuditIdentity(assignment)
			if err != nil {
				return err
			}
			if err := requireAuditText(event, "attachment_kind", auditTagAssignmentKind); err != nil {
				return err
			}
			storedIdentity, err := auditNestedField(event, "attachment_identity")
			if err != nil || !auditRecordEqual(identity, storedIdentity) {
				return errors.New("reverse event assignment identity mismatch")
			}
			if err := requireAuditAbsent(event, auditPreField); err != nil {
				return err
			}
			post, err := auditNestedField(event, auditPostField)
			if err != nil || !auditRecordEqual(assignment, post) {
				return errors.New("reverse event assignment payload mismatch")
			}
		} else {
			definition, err := tagMergeSourceDefinition(transition.before, transition.preview.SourceTagID)
			if err != nil {
				return err
			}
			identity, err := attachedAuditIdentity(definition)
			if err != nil {
				return err
			}
			if err := requireAuditText(event, "attachment_kind", auditTagDefinitionKind); err != nil {
				return err
			}
			storedIdentity, err := auditNestedField(event, "attachment_identity")
			if err != nil || !auditRecordEqual(identity, storedIdentity) {
				return errors.New("merge event definition identity mismatch")
			}
			pre, err := auditNestedField(event, auditPreField)
			if err != nil || !auditRecordEqual(definition, pre) {
				return errors.New("merge event definition payload mismatch")
			}
			if err := requireAuditAbsent(event, auditPostField); err != nil {
				return err
			}
		}
	}
	if err := replay.validateMemberStateChanges(mutation.record, auditedTagCandidateNodeIDs(candidates)); err != nil {
		return err
	}
	if err := replay.advanceScopes(vaultID, mutation, scopeIDs, scopeEntries); err != nil {
		return err
	}
	if err := replay.advanceAllocation(vaultID, operationID, mutation, allocation, digest, 1); err != nil {
		return err
	}
	if err := replay.applyTagAffectedNodeState(auditedTagCandidateNodeIDs(candidates), &mutation.record); err != nil {
		return err
	}
	return replay.applyTagMergeSnapshotAfter(transition.after)
}

func requireNoChangeMutationFieldsExceptAttachment(mutation audit.Record) error {
	if err := requireAuditAbsentFields(mutation, auditTopologyDeltaField, "path_effect_digest", "witness_change_digest"); err != nil {
		return err
	}
	if err := requireAuditUnsigned(mutation, "path_effect_count", 0); err != nil {
		return err
	}
	if err := requireAuditUnsigned(mutation, auditWitnessChangeCountField, 0); err != nil {
		return err
	}
	baselines, err := auditRecordListField(mutation, "baselines")
	if err != nil {
		return err
	}
	if len(baselines) != 0 {
		return errors.New("tag merge cannot enroll a scope")
	}
	return nil
}
