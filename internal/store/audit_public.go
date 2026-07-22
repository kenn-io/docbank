package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"slices"

	"go.kenn.io/docbank/internal/audit"
)

// AuditEnrollmentPreview is the exact retention boundary a caller reviews
// before permanently enabling one audit scope in a vault.
type AuditEnrollmentPreview struct {
	InitialAuthority       bool
	VaultID                string
	ScopeID                string
	OperationID            string
	TargetNodeID           int64
	TargetPath             string
	BaselineDigest         string
	MemberCount            int
	FileCount              int
	DirectoryCount         int
	VersionCount           int
	LogicalVersionBytes    int64
	UniqueBlobs            int
	UniqueBlobBytes        int64
	UnresolvedTrashOrigins int
	VaultTopologyNodes     int
	VaultAttachmentRecords int
	AuthorityJSONBytes     int64
}

// AuditEnrollmentPlan carries the server-private, non-derivable inputs behind
// a preview. Its fields are intentionally opaque outside this package: callers
// may present the summary, but only Store can execute the exact plan.
type AuditEnrollmentPlan struct {
	preview                 AuditEnrollmentPreview
	input                   initialAuditEnrollmentInput
	initial                 bool
	allocationGenesisDigest string
	allocationHead          string
}

// Preview returns a copy of the plan's public retention inventory.
func (plan *AuditEnrollmentPlan) Preview() AuditEnrollmentPreview {
	if plan == nil {
		return AuditEnrollmentPreview{}
	}
	return plan.preview
}

// AuditScopeStatus is one permanent scope and its current chain evidence.
type AuditScopeStatus struct {
	ID                string
	TargetNodeID      int64
	TargetPath        string
	TargetTrashed     bool
	EnableOperationID string
	BaselineDigest    string
	MemberCount       int
	EntryCount        int64
	ChainHead         string
}

// AuditMembershipStatus explains whether one inspected node is protected and
// which permanent scope/baseline bindings provide that protection.
type AuditMembershipStatus struct {
	NodeID          int64
	Path            string
	Trashed         bool
	Protected       bool
	ScopeIDs        []string
	BaselineDigests []string
}

// AuditStatus is the vault-wide audit authority plus optional node membership.
type AuditStatus struct {
	Enabled                    bool
	EnabledScopeID             string
	VaultID                    string
	LineageID                  string
	OperationSequenceHighWater int64
	AllocationEntryCount       int64
	AllocationHead             string
	Scopes                     []AuditScopeStatus
	Membership                 *AuditMembershipStatus
}

// PreviewInitialAudit derives the exact next permanent scope without writing
// it. The first scope creates vault authority; later scopes must be disjoint.
func (s *Store) PreviewInitialAudit(
	ctx context.Context, targetNodeID int64, origin string, agentLabel *string,
) (*AuditEnrollmentPlan, error) {
	return s.previewInitialAudit(ctx, origin, agentLabel, func(_ *sql.Tx) (int64, error) {
		return targetNodeID, nil
	})
}

// PreviewInitialAuditPath resolves a live path and derives its exact
// enrollment boundary in the same database snapshot.
func (s *Store) PreviewInitialAuditPath(
	ctx context.Context, path, origin string, agentLabel *string,
) (*AuditEnrollmentPlan, error) {
	return s.previewInitialAudit(ctx, origin, agentLabel, func(tx *sql.Tx) (int64, error) {
		node, err := nodeByPath(ctx, tx, s.rootID, path)
		return node.ID, err
	})
}

func (s *Store) previewInitialAudit(
	ctx context.Context, origin string, agentLabel *string,
	resolveTarget func(*sql.Tx) (int64, error),
) (*AuditEnrollmentPlan, error) {
	var plan *AuditEnrollmentPlan
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		targetNodeID, err := resolveTarget(tx)
		if err != nil {
			return err
		}
		if err := validateLiveAuditEnrollmentTarget(tx, targetNodeID); err != nil {
			return err
		}
		targetPath, err := pathOf(ctx, tx, targetNodeID)
		if err != nil {
			return err
		}
		counts, err := auditAuthorityCounts(ctx, tx)
		if err != nil {
			return err
		}
		if counts == [5]int64{} {
			input, err := newInitialAuditEnrollmentInput(targetNodeID, origin, agentLabel)
			if err != nil {
				return err
			}
			set, err := buildInitialAuditEnrollment(ctx, tx, s.vaultID, input)
			if err != nil {
				return err
			}
			preview, err := summarizeInitialAuditEnrollment(s.vaultID, set, targetPath)
			if err != nil {
				return err
			}
			plan = &AuditEnrollmentPlan{
				preview: preview, input: input, initial: true,
				allocationGenesisDigest: set.allocationGenesisDigest,
			}
			return nil
		}
		if counts[0] != 1 || counts[1] < 1 || counts[2] < 1 || counts[3] < 1 || counts[4] < 8 {
			return errors.New("audit authority is incomplete")
		}
		authority, nodeSequence, err := loadAuditAuthorityTx(ctx, tx)
		if err != nil {
			return err
		}
		if err := validateAuditAuthority(ctx, tx, s.vaultID, nodeSequence); err != nil {
			return fmt.Errorf("validating existing audit authority: %w", err)
		}
		input, err := newAdditionalAuditEnrollmentInput(
			targetNodeID, origin, agentLabel, authority.lineageID,
		)
		if err != nil {
			return err
		}
		set, err := buildAdditionalAuditEnrollment(ctx, tx, s.vaultID, input)
		if err != nil {
			return err
		}
		preview, err := summarizeAdditionalAuditEnrollment(s.vaultID, set, targetPath)
		if err != nil {
			return err
		}
		plan = &AuditEnrollmentPlan{
			preview: preview, input: input, allocationHead: set.allocationHead,
		}
		return nil
	})
	return plan, err
}

// EnableInitialAudit commits a previously reviewed scope only when its
// baseline and allocation authority still exactly match current metadata.
func (s *Store) EnableInitialAudit(
	ctx context.Context, plan *AuditEnrollmentPlan,
) (AuditStatus, error) {
	if plan == nil || plan.preview.VaultID != s.vaultID || plan.input.targetNodeID <= 0 {
		return AuditStatus{}, fmt.Errorf("invalid audit enrollment plan: %w", ErrAuditPreviewStale)
	}
	var status AuditStatus
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if err := requireLiveAuditPreviewTarget(tx, plan.input.targetNodeID); err != nil {
			return err
		}
		if plan.initial {
			if err := requireDormantAuditAuthority(ctx, tx); err != nil {
				return err
			}
			set, err := buildInitialAuditEnrollment(ctx, tx, s.vaultID, plan.input)
			if err != nil {
				return err
			}
			if set.baselineDigest != plan.preview.BaselineDigest ||
				set.allocationGenesisDigest != plan.allocationGenesisDigest {
				return ErrAuditPreviewStale
			}
			if err := persistInitialAuditEnrollment(ctx, tx, set); err != nil {
				return err
			}
			if err := validateMetadataState(ctx, tx, set.nodeSequence); err != nil {
				return fmt.Errorf("validating created audit authority: %w", err)
			}
		} else {
			set, err := buildAdditionalAuditEnrollment(ctx, tx, s.vaultID, plan.input)
			if err != nil {
				if errors.Is(err, ErrAuditScopeOverlap) {
					return ErrAuditPreviewStale
				}
				return err
			}
			if set.baselineDigest != plan.preview.BaselineDigest ||
				set.allocationHead != plan.allocationHead {
				return ErrAuditPreviewStale
			}
			if err := persistAdditionalAuditEnrollment(ctx, tx, set); err != nil {
				return err
			}
			if err := validateMetadataState(ctx, tx, set.nodeSequence); err != nil {
				return fmt.Errorf("validating additional audit scope: %w", err)
			}
		}
		var err error
		status, err = auditStatusTx(ctx, tx, s.vaultID, nil)
		status.EnabledScopeID = plan.input.scopeID
		return err
	})
	return status, err
}

func requireLiveAuditPreviewTarget(tx *sql.Tx, targetNodeID int64) error {
	err := validateLiveAuditEnrollmentTarget(tx, targetNodeID)
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrNotDir) {
		return ErrAuditPreviewStale
	}
	return err
}

func validateLiveAuditEnrollmentTarget(tx *sql.Tx, targetNodeID int64) error {
	target, err := nodeByIDTx(tx, targetNodeID)
	if err != nil {
		return fmt.Errorf("audit enrollment target %d: %w", targetNodeID, err)
	}
	if target.TrashedAt != nil {
		return fmt.Errorf("audit enrollment target %d is trashed: %w", targetNodeID, ErrNotFound)
	}
	if !target.IsDir() {
		return fmt.Errorf("audit enrollment target %d: %w", targetNodeID, ErrNotDir)
	}
	return nil
}

// AuditStatus reports current authority and optionally the sticky membership
// of one stable node from one consistent database snapshot.
func (s *Store) AuditStatus(ctx context.Context, nodeID *int64) (AuditStatus, error) {
	return s.auditStatusSnapshot(ctx, func(_ *sql.Tx) (*int64, error) { return nodeID, nil })
}

// AuditStatusPath resolves one live path and its sticky membership in the same
// read snapshot as the vault-wide authority evidence.
func (s *Store) AuditStatusPath(ctx context.Context, path string) (AuditStatus, error) {
	return s.auditStatusSnapshot(ctx, func(tx *sql.Tx) (*int64, error) {
		node, err := nodeByPath(ctx, tx, s.rootID, path)
		if err != nil {
			return nil, err
		}
		return &node.ID, nil
	})
}

func (s *Store) auditStatusSnapshot(
	ctx context.Context, resolveNode func(*sql.Tx) (*int64, error),
) (AuditStatus, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return AuditStatus{}, fmt.Errorf("starting audit-status snapshot: %w", err)
	}
	nodeID, err := resolveNode(tx)
	if err != nil {
		_ = tx.Rollback()
		return AuditStatus{}, err
	}
	status, statusErr := auditStatusTx(ctx, tx, s.vaultID, nodeID)
	if statusErr != nil {
		_ = tx.Rollback()
		return AuditStatus{}, statusErr
	}
	if err := tx.Commit(); err != nil {
		return AuditStatus{}, fmt.Errorf("closing audit-status snapshot: %w", err)
	}
	return status, nil
}

func newInitialAuditEnrollmentInput(
	targetNodeID int64, origin string, agentLabel *string,
) (initialAuditEnrollmentInput, error) {
	scopeID, err := newUUIDv4()
	if err != nil {
		return initialAuditEnrollmentInput{}, err
	}
	operationID, err := newUUIDv4()
	if err != nil {
		return initialAuditEnrollmentInput{}, err
	}
	lineageID, err := newUUIDv4()
	if err != nil {
		return initialAuditEnrollmentInput{}, err
	}
	return initialAuditEnrollmentInput{
		targetNodeID: targetNodeID, scopeID: scopeID, operationID: operationID,
		lineageID: lineageID, recordedAt: nowRFC3339(), origin: origin,
		agentLabel: agentLabel,
	}, nil
}

func newAdditionalAuditEnrollmentInput(
	targetNodeID int64, origin string, agentLabel *string, lineageID string,
) (initialAuditEnrollmentInput, error) {
	input, err := newInitialAuditEnrollmentInput(targetNodeID, origin, agentLabel)
	if err != nil {
		return initialAuditEnrollmentInput{}, err
	}
	input.lineageID = lineageID
	return input, nil
}

func requireDormantAuditAuthority(ctx context.Context, tx metadataQuerier) error {
	counts, err := auditAuthorityCounts(ctx, tx)
	if err != nil {
		return err
	}
	if counts == [5]int64{} {
		return nil
	}
	if counts[0] == 1 && counts[1] > 0 {
		return ErrAuditAlreadyEnabled
	}
	return errors.New("audit authority is incomplete")
}

func summarizeInitialAuditEnrollment(
	vaultID string, set initialAuditEnrollmentSet, targetPath string,
) (AuditEnrollmentPreview, error) {
	if len(set.records) != 8 {
		return AuditEnrollmentPreview{}, errors.New("initial audit enrollment has an invalid record set")
	}
	topology, err := auditRecordListField(set.records[0], "nodes")
	if err != nil {
		return AuditEnrollmentPreview{}, err
	}
	attachments, err := auditRecordListField(set.records[1], "records")
	if err != nil {
		return AuditEnrollmentPreview{}, err
	}
	jsonBytes, err := initialAuditMetadataJSONBytes(set)
	if err != nil {
		return AuditEnrollmentPreview{}, err
	}
	return summarizeAuditEnrollment(
		vaultID, set.input, set.members, set.records[3], targetPath,
		true, len(topology), len(attachments), jsonBytes,
	)
}

func summarizeAdditionalAuditEnrollment(
	vaultID string, set additionalAuditEnrollmentSet, targetPath string,
) (AuditEnrollmentPreview, error) {
	if len(set.records) != 5 {
		return AuditEnrollmentPreview{}, errors.New("additional audit enrollment has an invalid record set")
	}
	jsonBytes, err := additionalAuditMetadataJSONBytes(set)
	if err != nil {
		return AuditEnrollmentPreview{}, err
	}
	return summarizeAuditEnrollment(
		vaultID, set.input, set.members, set.records[0], targetPath,
		false, 0, 0, jsonBytes,
	)
}

func summarizeAuditEnrollment(
	vaultID string, input initialAuditEnrollmentInput, members []uint64,
	baseline audit.Record, targetPath string, initial bool,
	topologyCount, attachmentCount int, jsonBytes int64,
) (AuditEnrollmentPreview, error) {
	baselineNodes, err := auditRecordListField(baseline, "nodes")
	if err != nil {
		return AuditEnrollmentPreview{}, err
	}
	versions, err := auditRecordListField(baseline, "versions")
	if err != nil {
		return AuditEnrollmentPreview{}, err
	}
	preview := AuditEnrollmentPreview{
		InitialAuthority: initial,
		VaultID:          vaultID, ScopeID: input.scopeID,
		OperationID: input.operationID, TargetNodeID: input.targetNodeID,
		TargetPath: targetPath, MemberCount: len(members), VersionCount: len(versions),
		VaultTopologyNodes: topologyCount, VaultAttachmentRecords: attachmentCount,
		AuthorityJSONBytes: jsonBytes,
	}
	digest, err := hashAuditRecord(baseline)
	if err != nil {
		return AuditEnrollmentPreview{}, err
	}
	preview.BaselineDigest = digest.text
	memberSet := auditMemberSet(members)
	for _, node := range baselineNodes {
		nodeID, err := auditUnsignedField(node, metadataNodeIDField)
		if err != nil {
			return AuditEnrollmentPreview{}, err
		}
		if !memberSet[nodeID] {
			continue
		}
		kind, err := auditTextField(node, "node_kind")
		if err != nil {
			return AuditEnrollmentPreview{}, err
		}
		switch kind {
		case nodeKindDir:
			preview.DirectoryCount++
		case "file":
			preview.FileCount++
		default:
			return AuditEnrollmentPreview{}, fmt.Errorf("audit preview contains node kind %q", kind)
		}
		origin, ok := audit.FieldValue(node, auditOriginField)
		if ok && !origin.IsAbsent() {
			record, nested := origin.RecordValue()
			if !nested {
				return AuditEnrollmentPreview{}, errors.New("audit preview contains invalid trash origin")
			}
			if record.Kind == "unknown_origin" {
				preview.UnresolvedTrashOrigins++
			}
		}
	}
	unique := make(map[string]int64)
	for _, version := range versions {
		size, err := auditUnsignedField(version, "size")
		if err != nil || size > math.MaxInt64 {
			return AuditEnrollmentPreview{}, errors.New("audit preview contains invalid version size")
		}
		if preview.LogicalVersionBytes > math.MaxInt64-int64(size) {
			return AuditEnrollmentPreview{}, errors.New("audit preview logical size overflows int64")
		}
		preview.LogicalVersionBytes += int64(size)
		hash, err := auditDigestField(version, "blob_hash")
		if err != nil {
			return AuditEnrollmentPreview{}, err
		}
		if prior, exists := unique[hash]; exists && prior != int64(size) {
			return AuditEnrollmentPreview{}, errors.New("audit preview contains inconsistent blob sizes")
		}
		unique[hash] = int64(size)
	}
	preview.UniqueBlobs = len(unique)
	for _, size := range unique {
		if preview.UniqueBlobBytes > math.MaxInt64-size {
			return AuditEnrollmentPreview{}, errors.New("audit preview unique size overflows int64")
		}
		preview.UniqueBlobBytes += size
	}
	return preview, nil
}

type metadataByteCounter struct{ total int64 }

func (counter *metadataByteCounter) Write(p []byte) (int, error) {
	size := int64(len(p))
	if counter.total > math.MaxInt64-size {
		return 0, errors.New("audit preview metadata size overflows int64")
	}
	counter.total += size
	return len(p), nil
}

func initialAuditMetadataJSONBytes(set initialAuditEnrollmentSet) (int64, error) {
	if set.input.targetNodeID <= 0 {
		return 0, errors.New("audit preview target node ID must be positive")
	}
	counter := new(metadataByteCounter)
	write := newMetadataJSONWriter(counter)
	input := set.input
	targetNodeID, err := positiveAuditNodeID(input.targetNodeID)
	if err != nil {
		return 0, err
	}
	if err := write(metadataAuditAuthority{
		Type: metadataAuditAuthorityType, LineageID: input.lineageID,
		OperationSequenceHighWater: 1,
		AllocationGenesisDigest:    set.allocationGenesisDigest,
		AllocationEntryCount:       1, AllocationHead: set.allocationHead,
	}); err != nil {
		return 0, err
	}
	if err := write(metadataAuditScope{
		Type: metadataAuditScopeType, ScopeID: input.scopeID,
		TargetNodeID: targetNodeID, EnableOperationID: input.operationID,
		EntryCount: 1, ChainHead: set.scopeHead,
	}); err != nil {
		return 0, err
	}
	for _, member := range set.members {
		if err := write(metadataAuditMembership{
			Type: metadataAuditMembershipType, ScopeID: input.scopeID,
			NodeID: member, BaselineDigest: set.baselineDigest,
		}); err != nil {
			return 0, err
		}
	}
	for _, record := range set.records {
		digest, err := hashAuditRecord(record)
		if err != nil {
			return 0, err
		}
		recordJSON, err := audit.MarshalJSONRecord(record)
		if err != nil {
			return 0, fmt.Errorf("encoding projected %s audit record: %w", record.Kind, err)
		}
		if err := write(metadataAuditRecord{
			Type: metadataAuditRecordType, Digest: digest.text, Record: recordJSON,
		}); err != nil {
			return 0, err
		}
	}
	return counter.total, nil
}

func additionalAuditMetadataJSONBytes(set additionalAuditEnrollmentSet) (int64, error) {
	input := set.input
	targetNodeID, err := positiveAuditNodeID(input.targetNodeID)
	if err != nil {
		return 0, err
	}
	nextSequence, err := nextAuditInteger("operation sequence", set.authority.sequence)
	if err != nil {
		return 0, err
	}
	nextCount, err := nextAuditInteger("allocation entry count", set.authority.allocationCount)
	if err != nil {
		return 0, err
	}
	auditNextSequence, err := positiveAuditInteger("operation sequence", nextSequence)
	if err != nil {
		return 0, err
	}
	auditNextCount, err := positiveAuditInteger("allocation entry count", nextCount)
	if err != nil {
		return 0, err
	}
	if set.authority.sequence < 1 || set.authority.allocationCount < 1 {
		return 0, errors.New("additional audit enrollment has invalid authority counters")
	}
	oldCounter := new(metadataByteCounter)
	if err := newMetadataJSONWriter(oldCounter)(metadataAuditAuthority{
		Type: metadataAuditAuthorityType, LineageID: set.authority.lineageID,
		OperationSequenceHighWater: uint64(set.authority.sequence),
		AllocationGenesisDigest:    set.authority.genesisDigest,
		AllocationEntryCount:       uint64(set.authority.allocationCount),
		AllocationHead:             set.authority.allocationHead,
	}); err != nil {
		return 0, err
	}
	counter := new(metadataByteCounter)
	write := newMetadataJSONWriter(counter)
	if err := write(metadataAuditAuthority{
		Type: metadataAuditAuthorityType, LineageID: set.authority.lineageID,
		OperationSequenceHighWater: auditNextSequence,
		AllocationGenesisDigest:    set.authority.genesisDigest,
		AllocationEntryCount:       auditNextCount, AllocationHead: set.allocationHead,
	}); err != nil {
		return 0, err
	}
	if err := write(metadataAuditScope{
		Type: metadataAuditScopeType, ScopeID: input.scopeID,
		TargetNodeID: targetNodeID, EnableOperationID: input.operationID,
		EntryCount: 1, ChainHead: set.scopeHead,
	}); err != nil {
		return 0, err
	}
	for _, member := range set.members {
		if err := write(metadataAuditMembership{
			Type: metadataAuditMembershipType, ScopeID: input.scopeID,
			NodeID: member, BaselineDigest: set.baselineDigest,
		}); err != nil {
			return 0, err
		}
	}
	for _, record := range set.records {
		digest, err := hashAuditRecord(record)
		if err != nil {
			return 0, err
		}
		recordJSON, err := audit.MarshalJSONRecord(record)
		if err != nil {
			return 0, fmt.Errorf("encoding projected %s audit record: %w", record.Kind, err)
		}
		if err := write(metadataAuditRecord{
			Type: metadataAuditRecordType, Digest: digest.text, Record: recordJSON,
		}); err != nil {
			return 0, err
		}
	}
	if counter.total <= oldCounter.total {
		return 0, errors.New("additional audit enrollment does not grow metadata")
	}
	return counter.total - oldCounter.total, nil
}

func auditStatusTx(
	ctx context.Context, tx *sql.Tx, vaultID string, nodeID *int64,
) (AuditStatus, error) {
	status := AuditStatus{VaultID: vaultID, Scopes: []AuditScopeStatus{}}
	err := tx.QueryRowContext(ctx, `SELECT lineage_id,operation_sequence_high_water,
		allocation_entry_count,allocation_head FROM audit_authority WHERE singleton=1`).Scan(
		&status.LineageID, &status.OperationSequenceHighWater,
		&status.AllocationEntryCount, &status.AllocationHead,
	)
	if errors.Is(err, sql.ErrNoRows) {
		if err := requireDormantAuditAuthority(ctx, tx); err != nil {
			return AuditStatus{}, err
		}
		if nodeID != nil {
			membership, err := auditMembershipStatusTx(ctx, tx, *nodeID)
			if err != nil {
				return AuditStatus{}, err
			}
			status.Membership = &membership
		}
		return status, nil
	}
	if err != nil {
		return AuditStatus{}, fmt.Errorf("reading audit authority status: %w", err)
	}
	status.Enabled = true
	rows, err := tx.QueryContext(ctx, `SELECT s.scope_id,s.target_node_id,
		s.enable_operation_id,b.digest,s.entry_count,s.chain_head,
		COUNT(m.node_id)
		FROM audit_scopes s
		JOIN audit_baselines b ON b.scope_id=s.scope_id AND b.operation_id=s.enable_operation_id
		JOIN audit_memberships m ON m.scope_id=s.scope_id
		GROUP BY s.scope_id,s.target_node_id,s.enable_operation_id,b.digest,
			s.entry_count,s.chain_head ORDER BY s.scope_id`)
	if err != nil {
		return AuditStatus{}, fmt.Errorf("reading audit scope status: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var scope AuditScopeStatus
		if err := rows.Scan(&scope.ID, &scope.TargetNodeID, &scope.EnableOperationID,
			&scope.BaselineDigest, &scope.EntryCount, &scope.ChainHead,
			&scope.MemberCount); err != nil {
			return AuditStatus{}, fmt.Errorf("scanning audit scope status: %w", err)
		}
		status.Scopes = append(status.Scopes, scope)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return AuditStatus{}, fmt.Errorf("reading audit scope status: %w", err)
	}
	if err := rows.Close(); err != nil {
		return AuditStatus{}, fmt.Errorf("closing audit scope status: %w", err)
	}
	if len(status.Scopes) == 0 {
		return AuditStatus{}, errors.New("audit authority lacks a scope")
	}
	for index := range status.Scopes {
		scope := &status.Scopes[index]
		target, err := nodeByIDTx(tx, scope.TargetNodeID)
		if err != nil {
			return AuditStatus{}, err
		}
		scope.TargetTrashed = target.TrashedAt != nil
		if !scope.TargetTrashed {
			scope.TargetPath, err = pathOf(ctx, tx, scope.TargetNodeID)
			if err != nil {
				return AuditStatus{}, err
			}
		}
	}
	if nodeID != nil {
		membership, err := auditMembershipStatusTx(ctx, tx, *nodeID)
		if err != nil {
			return AuditStatus{}, err
		}
		status.Membership = &membership
	}
	return status, nil
}

func auditMembershipStatusTx(
	ctx context.Context, tx *sql.Tx, nodeID int64,
) (AuditMembershipStatus, error) {
	node, err := nodeByIDTx(tx, nodeID)
	if err != nil {
		return AuditMembershipStatus{}, err
	}
	status := AuditMembershipStatus{
		NodeID: nodeID, Trashed: node.TrashedAt != nil,
		ScopeIDs: []string{}, BaselineDigests: []string{},
	}
	if !status.Trashed {
		status.Path, err = pathOf(ctx, tx, nodeID)
		if err != nil {
			return AuditMembershipStatus{}, err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT scope_id,baseline_digest
		FROM audit_memberships WHERE node_id=? ORDER BY scope_id`, nodeID)
	if err != nil {
		return AuditMembershipStatus{}, fmt.Errorf("reading node audit membership: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var scopeID, digest string
		if err := rows.Scan(&scopeID, &digest); err != nil {
			return AuditMembershipStatus{}, fmt.Errorf("scanning node audit membership: %w", err)
		}
		status.ScopeIDs = append(status.ScopeIDs, scopeID)
		status.BaselineDigests = append(status.BaselineDigests, digest)
	}
	if err := rows.Err(); err != nil {
		return AuditMembershipStatus{}, fmt.Errorf("reading node audit membership: %w", err)
	}
	status.Protected = len(status.ScopeIDs) != 0
	if !slices.IsSorted(status.ScopeIDs) {
		return AuditMembershipStatus{}, errors.New("audit membership scopes are not ordered")
	}
	return status, nil
}
