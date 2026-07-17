package audit

import (
	"bytes"
	"errors"
	"fmt"
)

type collectionPolicy byte

const (
	collectionNone collectionPolicy = iota
	collectionUnsigned
	collectionAllocatedNodeID
	collectionNodeID
	collectionVersion
	collectionAttachedRecord
	collectionWitness
	collectionTopologyChange
	collectionPathEffect
	collectionWitnessChange
	collectionAttachedChange
	collectionEvent
	collectionBaselineBinding
)

type orderPartKind byte

const (
	orderUnsigned orderPartKind = iota
	orderBytes
)

type orderPart struct {
	kind     orderPartKind
	present  bool
	unsigned uint64
	data     []byte
}

type collectionKey struct {
	sort   []orderPart
	unique []orderPart
}

func validateCollection(items []Value, policy collectionPolicy, path string) error {
	if policy == collectionNone {
		return nil
	}
	seen := make(map[string]struct{}, len(items))
	var previous []orderPart
	for index, item := range items {
		key, err := collectionKeyFor(policy, item)
		if err != nil {
			return fmt.Errorf("field %s[%d]: %w", path, index, err)
		}
		unique := key.unique
		if unique == nil {
			unique = key.sort
		}
		encodedUnique := encodeOrderKey(unique)
		if _, exists := seen[encodedUnique]; exists {
			return fmt.Errorf("field %s contains duplicate canonical collection key at index %d", path, index)
		}
		seen[encodedUnique] = struct{}{}
		if key.sort != nil && previous != nil && compareOrderKey(previous, key.sort) > 0 {
			return fmt.Errorf("field %s is not in canonical order at index %d", path, index)
		}
		previous = key.sort
	}
	return nil
}

func collectionKeyFor(policy collectionPolicy, item Value) (collectionKey, error) {
	switch policy {
	case collectionUnsigned:
		part := unsignedPart(item)
		return collectionKey{sort: []orderPart{part}}, nil
	case collectionAllocatedNodeID:
		return collectionKey{unique: []orderPart{unsignedPart(item)}}, nil
	case collectionNodeID:
		record, err := collectionRecord(item)
		if err != nil {
			return collectionKey{}, err
		}
		part, err := recordUnsignedPart(record, "node_id")
		return collectionKey{sort: []orderPart{part}}, err
	case collectionVersion:
		return versionCollectionKey(item)
	case collectionAttachedRecord:
		return attachedRecordCollectionKey(item)
	case collectionWitness:
		return witnessCollectionKey(item)
	case collectionTopologyChange:
		return topologyChangeCollectionKey(item)
	case collectionPathEffect:
		return pathEffectCollectionKey(item)
	case collectionWitnessChange:
		return witnessChangeCollectionKey(item)
	case collectionAttachedChange:
		return attachedChangeCollectionKey(item)
	case collectionEvent:
		return eventCollectionKey(item)
	case collectionBaselineBinding:
		return baselineBindingCollectionKey(item)
	case collectionNone:
		return collectionKey{}, nil
	default:
		return collectionKey{}, fmt.Errorf("unknown canonical collection policy %d", policy)
	}
}

func versionCollectionKey(item Value) (collectionKey, error) {
	record, err := collectionRecord(item)
	if err != nil {
		return collectionKey{}, err
	}
	nodeID, err := recordUnsignedPart(record, "node_id")
	if err != nil {
		return collectionKey{}, err
	}
	versionID, err := recordDataPart(record, "version_id")
	if err != nil {
		return collectionKey{}, err
	}
	return collectionKey{sort: []orderPart{nodeID, versionID}}, nil
}

func attachedRecordCollectionKey(item Value) (collectionKey, error) {
	record, err := collectionRecord(item)
	if err != nil {
		return collectionKey{}, err
	}
	identity, err := attachedRecordIdentity(record)
	if err != nil {
		return collectionKey{}, err
	}
	return collectionKey{sort: []orderPart{dataPart([]byte(record.Kind)), dataPart(identity)}}, nil
}

func witnessCollectionKey(item Value) (collectionKey, error) {
	record, err := collectionRecord(item)
	if err != nil {
		return collectionKey{}, err
	}
	nodeID, err := recordUnsignedPart(record, "node_id")
	if err != nil {
		return collectionKey{}, err
	}
	generation, err := recordDataPart(record, "generation_operation_id")
	if err != nil {
		return collectionKey{}, err
	}
	return collectionKey{sort: []orderPart{nodeID, generation}}, nil
}

func topologyChangeCollectionKey(item Value) (collectionKey, error) {
	record, err := collectionRecord(item)
	if err != nil {
		return collectionKey{}, err
	}
	nodeID, err := recordUnsignedPart(record, "node_id")
	if err != nil {
		return collectionKey{}, err
	}
	pre, err := recordNestedPart(record, "pre")
	if err != nil {
		return collectionKey{}, err
	}
	post, err := recordNestedPart(record, "post")
	if err != nil {
		return collectionKey{}, err
	}
	return collectionKey{
		sort:   []orderPart{nodeID, pre, post},
		unique: []orderPart{nodeID},
	}, nil
}

func pathEffectCollectionKey(item Value) (collectionKey, error) {
	record, err := collectionRecord(item)
	if err != nil {
		return collectionKey{}, err
	}
	scopeID, err := recordDataPart(record, "scope_id")
	if err != nil {
		return collectionKey{}, err
	}
	memberID, err := recordUnsignedPart(record, "member_node_id")
	if err != nil {
		return collectionKey{}, err
	}
	oldState, err := recordNested(record, "old")
	if err != nil {
		return collectionKey{}, err
	}
	newState, err := recordNested(record, "new")
	if err != nil {
		return collectionKey{}, err
	}
	oldPath, err := recordDataPart(oldState, "path")
	if err != nil {
		return collectionKey{}, err
	}
	newPath, err := recordDataPart(newState, "path")
	if err != nil {
		return collectionKey{}, err
	}
	oldCode, err := recordDataPart(oldState, "state")
	if err != nil {
		return collectionKey{}, err
	}
	newCode, err := recordDataPart(newState, "state")
	if err != nil {
		return collectionKey{}, err
	}
	return collectionKey{
		sort:   []orderPart{scopeID, memberID, oldPath, newPath, oldCode, newCode},
		unique: []orderPart{scopeID, memberID},
	}, nil
}

func witnessChangeCollectionKey(item Value) (collectionKey, error) {
	record, err := collectionRecord(item)
	if err != nil {
		return collectionKey{}, err
	}
	nodeID, err := recordUnsignedPart(record, "node_id")
	if err != nil {
		return collectionKey{}, err
	}
	generation, err := recordDataPart(record, "generation_operation_id")
	if err != nil {
		return collectionKey{}, err
	}
	action, err := recordDataPart(record, "action")
	if err != nil {
		return collectionKey{}, err
	}
	return collectionKey{sort: []orderPart{nodeID, generation, action}}, nil
}

func attachedChangeCollectionKey(item Value) (collectionKey, error) {
	record, err := collectionRecord(item)
	if err != nil {
		return collectionKey{}, err
	}
	recordKind, err := recordDataPart(record, "record_kind")
	if err != nil {
		return collectionKey{}, err
	}
	identity, err := recordNestedPart(record, "stable_identity")
	if err != nil {
		return collectionKey{}, err
	}
	return collectionKey{sort: []orderPart{recordKind, identity}}, nil
}

func eventCollectionKey(item Value) (collectionKey, error) {
	record, err := collectionRecord(item)
	if err != nil {
		return collectionKey{}, err
	}
	nodeID, err := recordUnsignedPart(record, "node_id")
	if err != nil {
		return collectionKey{}, err
	}
	eventKind, err := recordDataPart(record, "event_kind")
	if err != nil {
		return collectionKey{}, err
	}
	scopeID, err := recordDataPart(record, "scope_id")
	if err != nil {
		return collectionKey{}, err
	}
	targetID, err := recordOptionalUnsignedPart(record, "target_node_id")
	if err != nil {
		return collectionKey{}, err
	}
	attachmentKind, err := recordOptionalDataPart(record, "attachment_kind")
	if err != nil {
		return collectionKey{}, err
	}
	attachmentIdentity, err := recordNestedPart(record, "attachment_identity")
	if err != nil {
		return collectionKey{}, err
	}
	return collectionKey{sort: []orderPart{
		nodeID, eventKind, scopeID, targetID, attachmentKind, attachmentIdentity,
	}}, nil
}

func baselineBindingCollectionKey(item Value) (collectionKey, error) {
	record, err := collectionRecord(item)
	if err != nil {
		return collectionKey{}, err
	}
	scopeID, err := recordDataPart(record, "scope_id")
	if err != nil {
		return collectionKey{}, err
	}
	targetID, err := recordUnsignedPart(record, "target_node_id")
	if err != nil {
		return collectionKey{}, err
	}
	digest, err := recordDataPart(record, "baseline_digest")
	if err != nil {
		return collectionKey{}, err
	}
	return collectionKey{sort: []orderPart{scopeID, targetID, digest}}, nil
}

func attachedRecordIdentity(record *Record) ([]byte, error) {
	var identity Record
	switch record.Kind {
	case "ingest":
		ingestID, err := recordField(record, "ingest_id")
		if err != nil {
			return nil, err
		}
		identity = Record{Kind: "ingest_identity", Fields: []Field{{Name: "ingest_id", Value: ingestID}}}
	case "provenance":
		value, err := recordField(record, "identity")
		if err != nil {
			return nil, err
		}
		identity = Record{Kind: "provenance_identity_ref", Fields: []Field{{Name: "identity", Value: value}}}
	case "tag_assignment":
		tagID, err := recordField(record, "tag_id")
		if err != nil {
			return nil, err
		}
		nodeID, err := recordField(record, "node_id")
		if err != nil {
			return nil, err
		}
		identity = Record{Kind: "tag_assignment_identity", Fields: []Field{
			{Name: "tag_id", Value: tagID},
			{Name: "node_id", Value: nodeID},
		}}
	case "tag_definition":
		tagID, err := recordField(record, "tag_id")
		if err != nil {
			return nil, err
		}
		identity = Record{Kind: "tag_definition_identity", Fields: []Field{{Name: "tag_id", Value: tagID}}}
	default:
		return nil, fmt.Errorf("record kind %q has no attached-metadata identity", record.Kind)
	}
	return encodeCanonical(identity)
}

func collectionRecord(value Value) (*Record, error) {
	if value.kind != kindRecord || value.record == nil {
		return nil, errors.New("canonical collection item is not a nested record")
	}
	return value.record, nil
}

func recordNested(record *Record, name string) (*Record, error) {
	value, err := recordField(record, name)
	if err != nil {
		return nil, err
	}
	return collectionRecord(value)
}

func recordField(record *Record, name string) (Value, error) {
	for _, field := range record.Fields {
		if field.Name == name {
			return field.Value, nil
		}
	}
	return Value{}, fmt.Errorf("audit record %s lacks field %s", record.Kind, name)
}

func recordUnsignedPart(record *Record, name string) (orderPart, error) {
	value, err := recordField(record, name)
	if err != nil {
		return orderPart{}, err
	}
	if value.kind != kindUnsigned {
		return orderPart{}, fmt.Errorf("audit record %s.%s is not unsigned", record.Kind, name)
	}
	return unsignedPart(value), nil
}

func recordOptionalUnsignedPart(record *Record, name string) (orderPart, error) {
	value, err := recordField(record, name)
	if err != nil {
		return orderPart{}, err
	}
	if value.kind == kindAbsent {
		return absentPart(orderUnsigned), nil
	}
	if value.kind != kindUnsigned {
		return orderPart{}, fmt.Errorf("audit record %s.%s is not optional unsigned", record.Kind, name)
	}
	return unsignedPart(value), nil
}

func recordDataPart(record *Record, name string) (orderPart, error) {
	value, err := recordField(record, name)
	if err != nil {
		return orderPart{}, err
	}
	if value.kind != kindBytes && value.kind != kindText && value.kind != kindUUID && value.kind != kindDigest {
		return orderPart{}, fmt.Errorf("audit record %s.%s is not byte-comparable", record.Kind, name)
	}
	return dataPart(value.data), nil
}

func recordOptionalDataPart(record *Record, name string) (orderPart, error) {
	value, err := recordField(record, name)
	if err != nil {
		return orderPart{}, err
	}
	if value.kind == kindAbsent {
		return absentPart(orderBytes), nil
	}
	if value.kind != kindBytes && value.kind != kindText && value.kind != kindUUID && value.kind != kindDigest {
		return orderPart{}, fmt.Errorf("audit record %s.%s is not optional byte-comparable", record.Kind, name)
	}
	return dataPart(value.data), nil
}

func recordNestedPart(record *Record, name string) (orderPart, error) {
	value, err := recordField(record, name)
	if err != nil {
		return orderPart{}, err
	}
	if value.kind == kindAbsent {
		return absentPart(orderBytes), nil
	}
	nested, err := collectionRecord(value)
	if err != nil {
		return orderPart{}, err
	}
	encoded, err := encodeCanonical(*nested)
	if err != nil {
		return orderPart{}, err
	}
	return dataPart(encoded), nil
}

func unsignedPart(value Value) orderPart {
	return orderPart{kind: orderUnsigned, present: true, unsigned: value.unsigned}
}

func dataPart(value []byte) orderPart {
	return orderPart{kind: orderBytes, present: true, data: value}
}

func absentPart(kind orderPartKind) orderPart {
	return orderPart{kind: kind}
}

func compareOrderKey(left, right []orderPart) int {
	for index := range left {
		if result := compareOrderPart(left[index], right[index]); result != 0 {
			return result
		}
	}
	return 0
}

func compareOrderPart(left, right orderPart) int {
	if left.present != right.present {
		if left.present {
			return 1
		}
		return -1
	}
	if !left.present {
		return 0
	}
	if left.kind == orderUnsigned {
		if left.unsigned < right.unsigned {
			return -1
		}
		if left.unsigned > right.unsigned {
			return 1
		}
		return 0
	}
	return bytes.Compare(left.data, right.data)
}

func encodeOrderKey(parts []orderPart) string {
	encoded := make([]byte, 0, len(parts)*10)
	for _, part := range parts {
		encoded = append(encoded, byte(part.kind))
		if !part.present {
			encoded = append(encoded, 0)
			continue
		}
		encoded = append(encoded, 1)
		if part.kind == orderUnsigned {
			appendUint64(&encoded, part.unsigned)
		} else {
			appendFrame(&encoded, part.data)
		}
	}
	return string(encoded)
}
