package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	productionservice "go.kenn.io/docbank/internal/production"
)

// ProductionReviewRequest declares that one exact occurrence binding has been
// completely reviewed. Binding is a claim checked against stored authority.
type ProductionReviewRequest struct {
	OperationID string `json:"operation_id"`
	ETag        int64  `json:"etag"`
	MemberID    string `json:"member_id"`
	Binding     string `json:"binding"`
	Complete    bool   `json:"complete"`
}

type productionMutationReceiptV1 struct {
	Version int               `json:"version"`
	Kind    string            `json:"kind"`
	Receipt redaction.Receipt `json:"receipt"`
	Draft   *redaction.Draft  `json:"draft,omitzero"`
}

func productionRowsResult(iterationErr, closeErr error) error {
	return errors.Join(iterationErr, closeErr)
}

func (s *Store) ApplyProductionChanges(ctx context.Context, actor, setID string, revision int64, request redaction.ApplyRequest) (redaction.Receipt, error) {
	if !validProductionActor(actor) || validateUUIDv4(setID) != nil || revision < 1 || redaction.ValidateApplyRequest(request) != nil {
		return redaction.Receipt{}, ErrInvalidProduction
	}
	raw, err := canonicalProductionOperationRequest("apply", setID, revision, request)
	if err != nil || len(raw) > redaction.MaxCommandBytes {
		return redaction.Receipt{}, errors.Join(ErrInvalidProduction, err)
	}
	requestSHA256 := productionSHA256(raw)
	var receipt redaction.Receipt
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		stored, _, found, err := loadProductionMutationOperationTx(ctx, tx, request.OperationID, "apply", setID, requestSHA256)
		if err != nil {
			return err
		}
		if found {
			receipt = stored
			return nil
		}
		draft, err := reserveProductionDraftTx(ctx, tx, setID, revision, request.ETag)
		if err != nil {
			return err
		}
		existingByID, err := loadProductionTouchedMembersTx(ctx, tx, setID, revision, request.Changes)
		if err != nil {
			return err
		}
		if err := stageProductionMemberOrdinalsTx(ctx, tx, setID, revision, request.Changes, existingByID); err != nil {
			return err
		}
		createdAt := nowRFC3339()
		invalidateAllReviews := false
		invalidateMembers := make(map[string]struct{})
		// Structural member mutations run as one first phase so temporary
		// ordinals used for swaps are never observed by dependent changes.
		for phase := range 2 {
			for _, change := range request.Changes {
				memberPhase := change.Kind == "member" || change.Kind == "member_remove"
				if memberPhase != (phase == 0) {
					continue
				}
				if err := s.applyProductionChangeTx(ctx, tx, draft, change, actor, createdAt, existingByID); err != nil {
					return err
				}
				switch change.Kind {
				case "member":
					if existing, ok := existingByID[change.Member.ID]; !ok || !productionMemberMembershipEqual(existing, *change.Member) {
						invalidateAllReviews = true
					} else if existing.Mode != change.Member.Mode {
						invalidateAllReviews = true
					}
				case "member_remove", "recipe":
					invalidateAllReviews = true
				case "decision":
					invalidateMembers[change.Decision.MemberID] = struct{}{}
				case "decision_remove":
					// The removed row is gone; applyProductionChangeTx already clears
					// its member declaration without scanning staged ordinals.
				case "mode":
					invalidateAllReviews = true
				}
			}
		}
		if invalidateAllReviews {
			if err := invalidateProductionReviewsTx(ctx, tx, setID, revision, ""); err != nil {
				return err
			}
		} else {
			for memberID := range invalidateMembers {
				if err := invalidateProductionReviewsTx(ctx, tx, setID, revision, memberID); err != nil {
					return err
				}
			}
		}
		memberHash, _, err := productionMemberHashTx(ctx, tx, setID, revision)
		if err != nil {
			return err
		}
		decisionsSHA256, err := productionDecisionHashTx(ctx, tx, setID, revision)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE production_revisions SET member_hash=?,decisions_sha256=? WHERE set_id=? AND revision=?`, memberHash, decisionsSHA256, setID, revision); err != nil {
			return err
		}
		receipt = redaction.Receipt{OperationID: request.OperationID, SetID: setID, Revision: revision, ETag: draft.ETag, RequestSHA256: requestSHA256}
		storedDraft, err := scanProductionDraft(tx.QueryRowContext(ctx, productionDraftSelect+` WHERE set_id=? AND revision=?`, setID, revision))
		if err != nil {
			return err
		}
		return recordProductionMutationTx(ctx, tx, actor, "apply", receipt, &storedDraft)
	})
	return receipt, err
}

func (s *Store) EditProductionInstructions(ctx context.Context, actor, setID string, revision int64, request redaction.InstructionsEditRequest) (redaction.Receipt, error) {
	if !validProductionActor(actor) || validateUUIDv4(setID) != nil || revision < 1 || redaction.ValidateInstructionsEditRequest(request) != nil {
		return redaction.Receipt{}, ErrInvalidProduction
	}
	raw, err := canonicalProductionOperationRequest("instructions", setID, revision, request)
	if err != nil || len(raw) > redaction.MaxCommandBytes {
		return redaction.Receipt{}, errors.Join(ErrInvalidProduction, err)
	}
	requestSHA256 := productionSHA256(raw)
	var receipt redaction.Receipt
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		stored, _, found, err := loadProductionMutationOperationTx(ctx, tx, request.OperationID, "instructions", setID, requestSHA256)
		if err != nil {
			return err
		}
		if found {
			receipt = stored
			return nil
		}
		draft, err := reserveProductionDraftTx(ctx, tx, setID, revision, request.ETag)
		if err != nil {
			return err
		}
		if err := invalidateProductionReviewsTx(ctx, tx, setID, revision, ""); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE production_revisions SET instructions=?,instructions_sha256=? WHERE set_id=? AND revision=?`, []byte(request.Instructions), productionSHA256([]byte(request.Instructions)), setID, revision); err != nil {
			return err
		}
		receipt = redaction.Receipt{OperationID: request.OperationID, SetID: setID, Revision: revision, ETag: draft.ETag, RequestSHA256: requestSHA256}
		storedDraft, err := scanProductionDraft(tx.QueryRowContext(ctx, productionDraftSelect+` WHERE set_id=? AND revision=?`, setID, revision))
		if err != nil {
			return err
		}
		return recordProductionMutationTx(ctx, tx, actor, "instructions", receipt, &storedDraft)
	})
	return receipt, err
}

func (s *Store) SealProductionMembership(ctx context.Context, actor, setID string, revision int64, request redaction.MembershipSealRequest) (redaction.Receipt, error) {
	if !validProductionActor(actor) || validateUUIDv4(setID) != nil || revision < 1 || redaction.ValidateMembershipSealRequest(request) != nil {
		return redaction.Receipt{}, ErrInvalidProduction
	}
	raw, err := canonicalProductionOperationRequest("seal_membership", setID, revision, request)
	if err != nil || len(raw) > redaction.MaxCommandBytes {
		return redaction.Receipt{}, errors.Join(ErrInvalidProduction, err)
	}
	requestSHA256 := productionSHA256(raw)
	var receipt redaction.Receipt
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		stored, _, found, err := loadProductionMutationOperationTx(ctx, tx, request.OperationID, "seal_membership", setID, requestSHA256)
		if err != nil {
			return err
		}
		if found {
			receipt = stored
			return nil
		}
		draft, err := reserveProductionDraftTx(ctx, tx, setID, revision, request.ETag)
		if err != nil {
			return err
		}
		if draft.MembershipSealed || draft.MemberHash != request.MemberHash {
			return ErrProductionRevisionConflict
		}
		actualMemberHash, total, err := productionMemberHashTx(ctx, tx, setID, revision)
		if err != nil {
			return err
		}
		if total != request.Total || actualMemberHash != draft.MemberHash {
			return ErrProductionRevisionConflict
		}
		if _, err := tx.ExecContext(ctx, `UPDATE production_revisions SET membership_sealed=1 WHERE set_id=? AND revision=?`, setID, revision); err != nil {
			return err
		}
		receipt = redaction.Receipt{OperationID: request.OperationID, SetID: setID, Revision: revision, ETag: draft.ETag, RequestSHA256: requestSHA256}
		storedDraft, err := scanProductionDraft(tx.QueryRowContext(ctx, productionDraftSelect+` WHERE set_id=? AND revision=?`, setID, revision))
		if err != nil {
			return err
		}
		return recordProductionMutationTx(ctx, tx, actor, "seal_membership", receipt, &storedDraft)
	})
	return receipt, err
}

func (s *Store) ForkProductionDraft(ctx context.Context, actor, setID string, fromRevision int64, operationID string) (redaction.Draft, error) {
	if !validProductionActor(actor) || validateUUIDv4(setID) != nil || fromRevision < 1 || validateUUIDv4(operationID) != nil {
		return redaction.Draft{}, ErrInvalidProduction
	}
	request := struct {
		OperationID string `json:"operation_id"`
	}{operationID}
	raw, err := canonicalProductionOperationRequest("fork", setID, fromRevision, request)
	if err != nil {
		return redaction.Draft{}, err
	}
	requestSHA256 := productionSHA256(raw)
	var result redaction.Draft
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		_, storedDraft, found, err := loadProductionMutationOperationTx(ctx, tx, operationID, "fork", setID, requestSHA256)
		if err != nil {
			return err
		}
		if found {
			if storedDraft == nil {
				return ErrInvalidProduction
			}
			result = *storedDraft
			return nil
		}
		source, err := scanProductionDraft(tx.QueryRowContext(ctx, productionDraftSelect+` WHERE set_id=? AND revision=?`, setID, fromRevision))
		if err != nil {
			return err
		}
		var instructions []byte
		if err := tx.QueryRowContext(ctx, `SELECT instructions FROM production_revisions WHERE set_id=? AND revision=?`, setID, fromRevision).Scan(&instructions); err != nil {
			return err
		}
		update, err := tx.ExecContext(ctx, `UPDATE production_sets SET head_revision=head_revision+1 WHERE id=?`, setID)
		if err != nil {
			return err
		}
		if affected, _ := update.RowsAffected(); affected != 1 {
			return ErrNotFound
		}
		var nextRevision int64
		if err := tx.QueryRowContext(ctx, `SELECT head_revision FROM production_sets WHERE id=?`, setID).Scan(&nextRevision); err != nil {
			return err
		}
		createdAt := nowRFC3339()
		result = source
		result.Revision, result.ETag, result.State, result.MembershipSealed = nextRevision, 1, "draft", false
		if _, err := tx.ExecContext(ctx, `INSERT INTO production_revisions(
			set_id,revision,predecessor_revision,state,etag,instructions,instructions_sha256,member_hash,decisions_sha256,membership_sealed,
			recipe_kind,recipe_id,recipe_sha256,profile_kind,profile_id,profile_sha256,
			disclosure_profile_kind,disclosure_profile_id,disclosure_profile_sha256,
			numbering_recipe_kind,numbering_recipe_id,numbering_recipe_sha256,
			policy_id,policy_version,policy_sha256,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			setID, nextRevision, fromRevision, result.State, result.ETag, instructions, result.InstructionsSHA256,
			result.MemberHash, result.DecisionsSHA256, result.MembershipSealed,
			"recipe", result.RecipeID, result.RecipeSHA256, "profile", result.ProfileID, result.ProfileSHA256,
			"disclosure", result.DisclosureProfileID, result.DisclosureProfileSHA256,
			"numbering", result.NumberingRecipeID, result.NumberingRecipeSHA256,
			result.Policy.PolicyID, result.Policy.Version, result.Policy.PolicySHA256, createdAt); err != nil {
			return err
		}
		if err := s.copyProductionMembersTx(ctx, tx, setID, fromRevision, nextRevision); err != nil {
			return err
		}
		if err := copyProductionDecisionsTx(ctx, tx, setID, fromRevision, nextRevision, actor, createdAt); err != nil {
			return err
		}
		result.DecisionsSHA256, err = productionDecisionHashTx(ctx, tx, setID, nextRevision)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE production_revisions SET decisions_sha256=? WHERE set_id=? AND revision=?`, result.DecisionsSHA256, setID, nextRevision); err != nil {
			return err
		}
		receipt := redaction.Receipt{OperationID: operationID, SetID: setID, Revision: nextRevision, ETag: 1, RequestSHA256: requestSHA256}
		return recordProductionMutationTx(ctx, tx, actor, "fork", receipt, &result)
	})
	return result, err
}

// ReviewProductionMember records a review declaration only when the supplied
// binding exactly matches the current stored member, decisions, resolved plan,
// instructions, recipe, and ordered membership authority.
func (s *Store) ReviewProductionMember(
	ctx context.Context, actor, setID string, revision int64, request ProductionReviewRequest,
) (redaction.Receipt, error) {
	if !validProductionActor(actor) || validateUUIDv4(setID) != nil || revision < 1 ||
		validateUUIDv4(request.OperationID) != nil || request.ETag < 1 || validateUUIDv4(request.MemberID) != nil ||
		!canonical.IsSHA256Hex(request.Binding) || !request.Complete {
		return redaction.Receipt{}, ErrInvalidProduction
	}
	raw, err := canonicalProductionOperationRequest("review", setID, revision, request)
	if err != nil || len(raw) > redaction.MaxCommandBytes {
		return redaction.Receipt{}, errors.Join(ErrInvalidProduction, err)
	}
	requestSHA256 := productionSHA256(raw)
	var receipt redaction.Receipt
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		stored, _, found, err := loadProductionMutationOperationTx(ctx, tx, request.OperationID, "review", setID, requestSHA256)
		if err != nil {
			return err
		}
		if found {
			receipt = stored
			return nil
		}
		draft, err := reserveProductionDraftTx(ctx, tx, setID, revision, request.ETag)
		if err != nil {
			return err
		}
		storedInputs, err := s.loadProductionInputsTx(ctx, tx, setID, revision)
		if err != nil {
			return err
		}
		var target *productionservice.StoredPreparedMember
		for index := range storedInputs.Members {
			if storedInputs.Members[index].Member.ID == request.MemberID {
				target = &storedInputs.Members[index]
				break
			}
		}
		if target == nil {
			return ErrInvalidProduction
		}
		_, decisionsSHA256, err := redaction.CanonicalDecisions(target.Decisions)
		if err != nil {
			return ErrInvalidProduction
		}
		_, resolvedSHA256, err := redaction.CanonicalResolved(target.Resolved)
		if err != nil || target.Resolved.SHA256 != resolvedSHA256 {
			return ErrInvalidProduction
		}
		binding, err := redaction.ReviewBinding(redaction.ReviewInput{
			SetID: draft.SetID, MemberID: target.Member.ID, VaultID: target.Member.VaultID,
			SourceVersionID: target.Member.SourceVersionID, Revision: draft.Revision,
			Ordinal: target.Member.Ordinal, NodeID: target.Member.NodeID, SourceSize: target.Member.SourceSize,
			PDFSize: target.Member.PDFSize, SourceSHA256: target.Member.SourceSHA256, PDFSHA256: target.Member.PDFSHA256,
			PageInventorySHA256: target.Member.PageInventorySHA256, MapSHA256: target.Member.MapSHA256,
			Mode: target.Member.Mode, MemberHash: draft.MemberHash, InstructionsSHA256: draft.InstructionsSHA256,
			RecipeSHA256: draft.RecipeSHA256, DecisionsSHA256: decisionsSHA256, ResolvedSHA256: resolvedSHA256,
		})
		if err != nil || binding != request.Binding {
			return ErrProductionRevisionConflict
		}
		target.Member.Reviewed, target.Member.ReviewBinding = true, binding
		if err := updateProductionMemberCanonicalTx(ctx, tx, setID, revision, target.Member); err != nil {
			return err
		}
		receipt = redaction.Receipt{OperationID: request.OperationID, SetID: setID, Revision: revision, ETag: draft.ETag, RequestSHA256: requestSHA256}
		storedDraft, err := scanProductionDraft(tx.QueryRowContext(ctx, productionDraftSelect+` WHERE set_id=? AND revision=?`, setID, revision))
		if err != nil {
			return err
		}
		return recordProductionMutationTx(ctx, tx, actor, "review", receipt, &storedDraft)
	})
	return receipt, err
}

// LoadProductionGateSnapshot is the concrete gate loader. The caller passes
// the writer transaction that will also retain the gate receipt; every source,
// rendition, frame, occurrence, decision, review, catalog, and policy fact is
// resolved through that transaction.
func (s *Store) LoadProductionGateSnapshot(
	ctx context.Context, tx *sql.Tx, request productionservice.PreparedInputRequest,
) (productionservice.StoredProductionInputs, error) {
	if s == nil || tx == nil || validateUUIDv4(request.SetID) != nil || request.Revision < 1 {
		return productionservice.StoredProductionInputs{}, ErrInvalidProduction
	}
	return s.loadProductionInputsTx(ctx, tx, request.SetID, request.Revision)
}

func (s *Store) loadProductionInputsTx(
	ctx context.Context, tx *sql.Tx, setID string, revision int64,
) (productionservice.StoredProductionInputs, error) {
	var stored productionservice.StoredProductionInputs
	var err error
	stored.Draft, err = scanProductionDraft(tx.QueryRowContext(ctx, productionDraftSelect+` WHERE set_id=? AND revision=?`, setID, revision))
	if err != nil {
		return stored, err
	}
	if err := validateProductionDraftCatalogTx(ctx, tx, stored.Draft); err != nil {
		return productionservice.StoredProductionInputs{}, err
	}
	stored.Policy, err = loadProductionPolicyByDigest(ctx, tx, stored.Draft.Policy.PolicySHA256)
	if err != nil || stored.Policy.ID != stored.Draft.Policy.PolicyID || stored.Policy.Version != stored.Draft.Policy.Version {
		return productionservice.StoredProductionInputs{}, errors.Join(ErrInvalidProduction, err)
	}
	withheld, found, err := loadProductionWithheldForRevisionTx(ctx, tx, setID, revision, stored.Policy.SHA256)
	if err != nil {
		return productionservice.StoredProductionInputs{}, err
	}
	if found {
		stored.Withheld = &withheld
	}
	recipe, err := loadProductionRecipeTx(ctx, tx, stored.Draft)
	if err != nil {
		return productionservice.StoredProductionInputs{}, err
	}
	members, err := loadAllProductionMembersTx(ctx, tx, setID, revision)
	if err != nil {
		return productionservice.StoredProductionInputs{}, err
	}
	decisions, err := loadAllProductionDecisionsTx(ctx, tx, setID, revision)
	if err != nil {
		return productionservice.StoredProductionInputs{}, err
	}
	memberHash, err := productionMemberHash(members)
	if err != nil || memberHash != stored.Draft.MemberHash {
		return productionservice.StoredProductionInputs{}, ErrInvalidProduction
	}
	allDecisions := make([]redaction.Decision, 0)
	stored.Members = make([]productionservice.StoredPreparedMember, len(members))
	for index, member := range members {
		mapAuthority, found, loadErr := loadProductionTextMapTx(ctx, tx, member.MapSHA256)
		if loadErr != nil || !found || mapAuthority.SourceNodeID != member.NodeID || mapAuthority.Source.VersionID != member.SourceVersionID ||
			mapAuthority.Source.SHA256 != member.SourceSHA256 || mapAuthority.Source.Size != member.SourceSize ||
			mapAuthority.PDFSHA256 != member.PDFSHA256 || mapAuthority.PDFSize != member.PDFSize {
			return productionservice.StoredProductionInputs{}, errors.Join(ErrInvalidProduction, loadErr)
		}
		memberDecisions := slices.Clone(decisions[member.ID])
		if memberDecisions == nil {
			memberDecisions = []redaction.Decision{}
		}
		resolved, resolveErr := redaction.Resolve(mapAuthority.Map, member.Mode, memberDecisions, recipe)
		if resolveErr != nil {
			return productionservice.StoredProductionInputs{}, resolveErr
		}
		labels := make([]string, 0, len(memberDecisions))
		for _, decision := range memberDecisions {
			if decision.Label != "" {
				labels = append(labels, decision.Label)
			}
		}
		slices.Sort(labels)
		labels = slices.Compact(labels)
		stored.Members[index] = productionservice.StoredPreparedMember{
			Member: member, Decisions: memberDecisions, Resolved: resolved,
			Facts: productionPolicyMemberFacts(member, labels),
		}
		allDecisions = append(allDecisions, memberDecisions...)
	}
	_, decisionsSHA256, err := redaction.CanonicalDecisions(allDecisions)
	if err != nil || decisionsSHA256 != stored.Draft.DecisionsSHA256 {
		return productionservice.StoredProductionInputs{}, ErrInvalidProduction
	}
	return stored, nil
}

func productionPolicyMemberFacts(member redaction.Member, labels []string) documentproduction.PolicyMemberFacts {
	// family.kind is the exact validated FamilyContext value. Do not translate
	// it into a policy vocabulary that storage cannot prove.
	return documentproduction.PolicyMemberFacts{
		MemberID: member.ID, FamilyID: member.Family.RootVersionID,
		FamilyComplete: member.Family.Kind == "standalone" || member.Family.Kind == "transcript",
		Fields:         map[string][]string{"family.kind": {member.Family.Kind}}, Labels: labels,
	}
}

func loadProductionWithheldForRevisionTx(
	ctx context.Context, tx *sql.Tx, setID string, revision int64, policySHA256 string,
) (documentproduction.WithheldSelection, bool, error) {
	var raw []byte
	var digest string
	err := tx.QueryRowContext(ctx, `SELECT canonical_json,sha256 FROM production_withheld_selections
		WHERE set_id=? AND revision=?`, setID, revision).Scan(&raw, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return documentproduction.WithheldSelection{}, false, nil
	}
	if err != nil {
		return documentproduction.WithheldSelection{}, false, err
	}
	selection, err := decodeWithheldSelection(raw, digest)
	if err != nil || selection.SetID != setID || selection.Revision != revision || selection.PolicySHA256 != policySHA256 {
		return documentproduction.WithheldSelection{}, false, errors.Join(ErrInvalidProduction, err)
	}
	return selection, true, nil
}

func loadProductionRecipeTx(ctx context.Context, tx *sql.Tx, draft redaction.Draft) (redaction.Recipe, error) {
	var raw []byte
	if err := tx.QueryRowContext(ctx, `SELECT canonical_json FROM production_catalog_entries
		WHERE kind='recipe' AND id=? AND sha256=?`, draft.RecipeID, draft.RecipeSHA256).Scan(&raw); err != nil {
		return redaction.Recipe{}, errors.Join(ErrInvalidProduction, err)
	}
	recipe, err := canonical.Decode[redaction.Recipe](raw)
	if err != nil || validateProductionCatalogEntry("recipe", draft.RecipeID, draft.RecipeSHA256, raw) != nil {
		return redaction.Recipe{}, errors.Join(ErrInvalidProduction, err)
	}
	return recipe, nil
}

func loadAllProductionMembersTx(ctx context.Context, tx *sql.Tx, setID string, revision int64) ([]redaction.Member, error) {
	result := make([]redaction.Member, 0)
	var lastOrdinal int64
	var lastMemberID string
	for {
		batch, err := func() (batch []redaction.Member, retErr error) {
			rows, err := tx.QueryContext(ctx, `SELECT member_id,ordinal,vault_id,node_id,version_id,source_sha256,source_size,pdf_sha256,pdf_size,
				map_sha256,page_inventory_sha256,family_context_json,canonical_json FROM production_members
				WHERE set_id=? AND revision=? AND (ordinal>? OR (ordinal=? AND member_id>?))
				ORDER BY ordinal,member_id LIMIT ?`, setID, revision, lastOrdinal, lastOrdinal, lastMemberID, redaction.MaxProductionPage)
			if err != nil {
				return nil, err
			}
			defer func() { retErr = productionRowsResult(retErr, rows.Close()) }()
			batch = make([]redaction.Member, 0, redaction.MaxProductionPage)
			for rows.Next() {
				member, err := scanProductionMember(rows)
				if err != nil {
					return nil, err
				}
				batch = append(batch, member)
			}
			return batch, rows.Err()
		}()
		if err != nil {
			return nil, err
		}
		result = append(result, batch...)
		if len(batch) < redaction.MaxProductionPage {
			return result, nil
		}
		last := batch[len(batch)-1]
		lastOrdinal, lastMemberID = last.Ordinal, last.ID
	}
}

func loadAllProductionDecisionsTx(
	ctx context.Context, tx *sql.Tx, setID string, revision int64,
) (map[string][]redaction.Decision, error) {
	result := make(map[string][]redaction.Decision)
	var lastMemberID, lastDecisionID string
	for {
		batch, err := func() (batch []redaction.Decision, retErr error) {
			rows, err := tx.QueryContext(ctx, `SELECT revision,decision_id,member_id,actor,created_at,canonical_json
				FROM production_decisions WHERE set_id=? AND revision=? AND
				(member_id>? OR (member_id=? AND decision_id>?)) ORDER BY member_id,decision_id LIMIT ?`,
				setID, revision, lastMemberID, lastMemberID, lastDecisionID, redaction.MaxProductionPage)
			if err != nil {
				return nil, err
			}
			defer func() { retErr = productionRowsResult(retErr, rows.Close()) }()
			batch = make([]redaction.Decision, 0, redaction.MaxProductionPage)
			for rows.Next() {
				decision, err := scanProductionDecision(rows)
				if err != nil {
					return nil, err
				}
				batch = append(batch, decision)
			}
			return batch, rows.Err()
		}()
		if err != nil {
			return nil, err
		}
		for _, decision := range batch {
			result[decision.MemberID] = append(result[decision.MemberID], decision)
		}
		if len(batch) < redaction.MaxProductionPage {
			return result, nil
		}
		last := batch[len(batch)-1]
		lastMemberID, lastDecisionID = last.MemberID, last.ID
	}
}

func (s *Store) copyProductionMembersTx(ctx context.Context, tx *sql.Tx, setID string, fromRevision, toRevision int64) error {
	var lastOrdinal int64
	var lastMemberID string
	for {
		members, err := func() ([]redaction.Member, error) {
			rows, err := tx.QueryContext(ctx, `SELECT member_id,ordinal,vault_id,node_id,version_id,source_sha256,source_size,pdf_sha256,pdf_size,
			map_sha256,page_inventory_sha256,family_context_json,canonical_json FROM production_members
			WHERE set_id=? AND revision=? AND (ordinal>? OR (ordinal=? AND member_id>?))
			ORDER BY ordinal,member_id LIMIT ?`, setID, fromRevision, lastOrdinal, lastOrdinal, lastMemberID, redaction.MaxProductionPage)
			if err != nil {
				return nil, err
			}
			defer func() { _ = rows.Close() }()
			members := make([]redaction.Member, 0, redaction.MaxProductionPage)
			for rows.Next() {
				member, err := scanProductionMember(rows)
				if err != nil {
					return nil, err
				}
				members = append(members, member)
			}
			return members, rows.Err()
		}()
		if err != nil {
			return err
		}
		for _, member := range members {
			member.Reviewed, member.ReviewBinding = false, ""
			if err := s.upsertProductionMemberTx(ctx, tx, setID, toRevision, member); err != nil {
				return err
			}
		}
		if len(members) < redaction.MaxProductionPage {
			return nil
		}
		last := members[len(members)-1]
		lastOrdinal, lastMemberID = last.Ordinal, last.ID
	}
}

func copyProductionDecisionsTx(ctx context.Context, tx *sql.Tx, setID string, fromRevision, toRevision int64, actor, createdAt string) error {
	var lastMemberID, lastDecisionID string
	for {
		decisions, err := func() ([]redaction.Decision, error) {
			rows, err := tx.QueryContext(ctx, `SELECT revision,decision_id,member_id,actor,created_at,canonical_json FROM production_decisions
			WHERE set_id=? AND revision=? AND (member_id>? OR (member_id=? AND decision_id>?))
			ORDER BY member_id,decision_id LIMIT ?`, setID, fromRevision, lastMemberID, lastMemberID, lastDecisionID, redaction.MaxProductionPage)
			if err != nil {
				return nil, err
			}
			defer func() { _ = rows.Close() }()
			decisions := make([]redaction.Decision, 0, redaction.MaxProductionPage)
			for rows.Next() {
				decision, err := scanProductionDecision(rows)
				if err != nil {
					return nil, err
				}
				decisions = append(decisions, decision)
			}
			return decisions, rows.Err()
		}()
		if err != nil {
			return err
		}
		for _, decision := range decisions {
			decision.Actor, decision.CreatedAt, decision.Revision = actor, createdAt, toRevision
			if err := upsertProductionDecisionTx(ctx, tx, setID, toRevision, decision); err != nil {
				return err
			}
		}
		if len(decisions) < redaction.MaxProductionPage {
			return nil
		}
		last := decisions[len(decisions)-1]
		lastMemberID, lastDecisionID = last.MemberID, last.ID
	}
}

func reserveProductionDraftTx(ctx context.Context, tx *sql.Tx, setID string, revision, etag int64) (redaction.Draft, error) {
	result, err := tx.ExecContext(ctx, `UPDATE production_revisions SET etag=etag+1
		WHERE set_id=? AND revision=? AND etag=? AND state='draft'`, setID, revision, etag)
	if err != nil {
		return redaction.Draft{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return redaction.Draft{}, ErrProductionRevisionConflict
	}
	return scanProductionDraft(tx.QueryRowContext(ctx, productionDraftSelect+` WHERE set_id=? AND revision=?`, setID, revision))
}

func (s *Store) applyProductionChangeTx(ctx context.Context, tx *sql.Tx, draft redaction.Draft, change redaction.Change, actor, createdAt string, existingByID map[string]redaction.Member) error {
	switch change.Kind {
	case "member":
		if draft.MembershipSealed {
			return ErrProductionRevisionConflict
		}
		if change.Member.Reviewed || change.Member.ReviewBinding != "" {
			return ErrInvalidProduction
		}
		member := *change.Member
		if existing, ok := existingByID[member.ID]; ok {
			if !productionMemberSelectorAuthorityEqual(existing, member) {
				if _, err := tx.ExecContext(ctx, `DELETE FROM production_decisions WHERE set_id=? AND revision=? AND member_id=?`, draft.SetID, draft.Revision, member.ID); err != nil {
					return err
				}
			}
			if productionMemberAuthorityEqual(existing, member) {
				member.Reviewed, member.ReviewBinding = existing.Reviewed, existing.ReviewBinding
			}
		}
		if err := s.upsertProductionMemberTx(ctx, tx, draft.SetID, draft.Revision, member); err != nil {
			return err
		}
		return nil
	case "member_remove":
		if draft.MembershipSealed {
			return ErrProductionRevisionConflict
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM production_members WHERE set_id=? AND revision=? AND member_id=?`, draft.SetID, draft.Revision, change.MemberID)
		if err != nil {
			return err
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return ErrInvalidProduction
		}
		return nil
	case "decision":
		decision := *change.Decision
		if decision.Actor != "" || decision.CreatedAt != "" || decision.Revision != 0 {
			return ErrInvalidProduction
		}
		decision.Actor, decision.CreatedAt, decision.Revision = actor, createdAt, draft.Revision
		if err := upsertProductionDecisionTx(ctx, tx, draft.SetID, draft.Revision, decision); err != nil {
			return err
		}
		return nil
	case "decision_remove":
		var memberID string
		if err := tx.QueryRowContext(ctx, `SELECT member_id FROM production_decisions WHERE set_id=? AND revision=? AND decision_id=?`, draft.SetID, draft.Revision, change.DecisionID).Scan(&memberID); err != nil {
			return ErrInvalidProduction
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM production_decisions WHERE set_id=? AND revision=? AND decision_id=?`, draft.SetID, draft.Revision, change.DecisionID); err != nil {
			return err
		}
		member, err := loadProductionMemberTx(ctx, tx, draft.SetID, draft.Revision, memberID)
		if err != nil {
			return err
		}
		member.Reviewed, member.ReviewBinding = false, ""
		return updateProductionMemberCanonicalTx(ctx, tx, draft.SetID, draft.Revision, member)
	case "mode":
		member, err := loadProductionMemberTx(ctx, tx, draft.SetID, draft.Revision, change.MemberID)
		if err != nil {
			return ErrInvalidProduction
		}
		member.Mode, member.Reviewed, member.ReviewBinding = change.Mode, false, ""
		return updateProductionMemberCanonicalTx(ctx, tx, draft.SetID, draft.Revision, member)
	case "recipe":
		selection, err := resolveProductionCatalog(change.RecipeID, draft.ProfileID, draft.DisclosureProfileID, draft.NumberingRecipeID)
		if err != nil {
			return err
		}
		if err := ensureProductionCatalogTx(ctx, tx, selection); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE production_revisions SET recipe_id=?,recipe_sha256=? WHERE set_id=? AND revision=?`, selection.recipeID, selection.recipeSHA256, draft.SetID, draft.Revision); err != nil {
			return err
		}
		return nil
	case "profile":
		selection, err := resolveProductionCatalog(draft.RecipeID, change.ProfileID, change.DisclosureProfileID, change.NumberingRecipeID)
		if err != nil {
			return err
		}
		if err := ensureProductionCatalogTx(ctx, tx, selection); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE production_revisions SET profile_id=?,profile_sha256=?,disclosure_profile_id=?,disclosure_profile_sha256=?,numbering_recipe_id=?,numbering_recipe_sha256=? WHERE set_id=? AND revision=?`,
			selection.profileID, selection.profileSHA256, selection.disclosureProfileID, selection.disclosureProfileSHA256,
			selection.numberingRecipeID, selection.numberingRecipeSHA256, draft.SetID, draft.Revision)
		return err
	case "policy":
		policy, selection, err := loadProductionPolicySelectionTx(ctx, tx, change.PolicyID, change.PolicyVersion)
		if err != nil {
			return err
		}
		if policy.ID != selection.PolicyID || policy.Version != selection.Version || policy.SHA256 != selection.PolicySHA256 {
			return ErrInvalidProduction
		}
		_, err = tx.ExecContext(ctx, `UPDATE production_revisions SET policy_id=?,policy_version=?,policy_sha256=?
			WHERE set_id=? AND revision=?`, selection.PolicyID, selection.Version, selection.PolicySHA256, draft.SetID, draft.Revision)
		return err
	default:
		return ErrInvalidProduction
	}
}

func stageProductionMemberOrdinalsTx(ctx context.Context, tx *sql.Tx, setID string, revision int64, changes []redaction.Change, existing map[string]redaction.Member) error {
	staged := make(map[string]struct{})
	for index, change := range changes {
		if change.Kind != "member" && change.Kind != "member_remove" {
			continue
		}
		memberID := change.MemberID
		if change.Member != nil {
			memberID = change.Member.ID
		}
		if _, ok := existing[memberID]; !ok {
			continue
		}
		if _, duplicate := staged[memberID]; duplicate {
			return ErrInvalidProduction
		}
		staged[memberID] = struct{}{}
		if _, err := tx.ExecContext(ctx, `UPDATE production_members SET ordinal=? WHERE set_id=? AND revision=? AND member_id=?`, -int64(index+1), setID, revision, memberID); err != nil {
			return err
		}
	}
	return nil
}

func loadProductionTouchedMembersTx(ctx context.Context, tx *sql.Tx, setID string, revision int64, changes []redaction.Change) (map[string]redaction.Member, error) {
	result := make(map[string]redaction.Member)
	for _, change := range changes {
		if change.Kind != "member" && change.Kind != "member_remove" {
			continue
		}
		memberID := change.MemberID
		if change.Member != nil {
			memberID = change.Member.ID
		}
		if _, loaded := result[memberID]; loaded {
			continue
		}
		member, err := loadProductionMemberTx(ctx, tx, setID, revision, memberID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		result[memberID] = member
	}
	return result, nil
}

func productionMemberAuthorityEqual(left, right redaction.Member) bool {
	left.Reviewed, left.ReviewBinding = false, ""
	right.Reviewed, right.ReviewBinding = false, ""
	return left == right
}

func productionMemberMembershipEqual(left, right redaction.Member) bool {
	left.Mode, right.Mode = "", ""
	return productionMemberAuthorityEqual(left, right)
}

func productionMemberSelectorAuthorityEqual(left, right redaction.Member) bool {
	left.ID, right.ID = "", ""
	left.Ordinal, right.Ordinal = 0, 0
	left.Mode, right.Mode = "", ""
	left.Reviewed, right.Reviewed = false, false
	left.ReviewBinding, right.ReviewBinding = "", ""
	return left == right
}

func loadProductionMemberTx(ctx context.Context, tx *sql.Tx, setID string, revision int64, memberID string) (redaction.Member, error) {
	return scanProductionMember(tx.QueryRowContext(ctx, `SELECT member_id,ordinal,vault_id,node_id,version_id,source_sha256,source_size,pdf_sha256,pdf_size,
		map_sha256,page_inventory_sha256,family_context_json,canonical_json FROM production_members
		WHERE set_id=? AND revision=? AND member_id=?`, setID, revision, memberID))
}

func invalidateProductionReviewsTx(ctx context.Context, tx *sql.Tx, setID string, revision int64, memberID string) error {
	if memberID != "" {
		member, err := loadProductionMemberTx(ctx, tx, setID, revision, memberID)
		if err != nil {
			return err
		}
		if !member.Reviewed && member.ReviewBinding == "" {
			return nil
		}
		member.Reviewed, member.ReviewBinding = false, ""
		return updateProductionMemberCanonicalTx(ctx, tx, setID, revision, member)
	}
	var lastOrdinal int64
	var lastMemberID string
	for {
		members, err := func() ([]redaction.Member, error) {
			rows, err := tx.QueryContext(ctx, `SELECT member_id,ordinal,vault_id,node_id,version_id,source_sha256,source_size,pdf_sha256,pdf_size,
			map_sha256,page_inventory_sha256,family_context_json,canonical_json FROM production_members
			WHERE set_id=? AND revision=? AND (ordinal>? OR (ordinal=? AND member_id>?))
			ORDER BY ordinal,member_id LIMIT ?`, setID, revision, lastOrdinal, lastOrdinal, lastMemberID, redaction.MaxProductionPage)
			if err != nil {
				return nil, err
			}
			defer func() { _ = rows.Close() }()
			members := make([]redaction.Member, 0, redaction.MaxProductionPage)
			for rows.Next() {
				member, err := scanProductionMember(rows)
				if err != nil {
					return nil, err
				}
				members = append(members, member)
			}
			return members, rows.Err()
		}()
		if err != nil {
			return err
		}
		for _, member := range members {
			if !member.Reviewed && member.ReviewBinding == "" {
				continue
			}
			member.Reviewed, member.ReviewBinding = false, ""
			if err := updateProductionMemberCanonicalTx(ctx, tx, setID, revision, member); err != nil {
				return err
			}
		}
		if len(members) < redaction.MaxProductionPage {
			return nil
		}
		last := members[len(members)-1]
		lastOrdinal, lastMemberID = last.Ordinal, last.ID
	}
}

func updateProductionMemberCanonicalTx(ctx context.Context, tx *sql.Tx, setID string, revision int64, member redaction.Member) error {
	if redaction.ValidateMember(member) != nil {
		return ErrInvalidProduction
	}
	raw, err := canonical.Marshal(member)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE production_members SET canonical_json=? WHERE set_id=? AND revision=? AND member_id=?`, raw, setID, revision, member.ID)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrInvalidProduction
	}
	return nil
}

func canonicalProductionOperationRequest(kind, setID string, revision int64, request any) ([]byte, error) {
	return canonical.Marshal(struct {
		Kind     string `json:"kind"`
		SetID    string `json:"set_id"`
		Revision int64  `json:"revision"`
		Request  any    `json:"request"`
	}{kind, setID, revision, request})
}

func loadProductionMutationOperationTx(ctx context.Context, tx *sql.Tx, operationID, kind, setID, requestSHA256 string) (redaction.Receipt, *redaction.Draft, bool, error) {
	var storedSetID, storedActor, storedKind, storedSHA256 string
	var raw []byte
	err := tx.QueryRowContext(ctx, `SELECT set_id,actor,kind,request_sha256,receipt_json FROM production_operations WHERE operation_id=?`, operationID).
		Scan(&storedSetID, &storedActor, &storedKind, &storedSHA256, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return redaction.Receipt{}, nil, false, nil
	}
	if err != nil {
		return redaction.Receipt{}, nil, false, err
	}
	if storedSetID != setID || storedKind != kind || storedSHA256 != requestSHA256 {
		return redaction.Receipt{}, nil, false, ErrProductionOperationConflict
	}
	value, err := canonical.Decode[productionMutationReceiptV1](raw)
	if err != nil || value.Version != 1 || value.Kind != kind || redaction.ValidateReceipt(value.Receipt) != nil ||
		value.Receipt.OperationID != operationID || value.Receipt.SetID != setID || value.Receipt.RequestSHA256 != requestSHA256 {
		return redaction.Receipt{}, nil, false, ErrInvalidProduction
	}
	if value.Draft != nil && (redaction.ValidateDraft(*value.Draft) != nil || value.Draft.SetID != setID ||
		value.Draft.Revision != value.Receipt.Revision || value.Draft.ETag != value.Receipt.ETag) {
		return redaction.Receipt{}, nil, false, ErrInvalidProduction
	}
	if value.Draft == nil {
		return redaction.Receipt{}, nil, false, ErrInvalidProduction
	}
	if validateProductionDraftCatalogTx(ctx, tx, *value.Draft) != nil || value.Draft.State != "draft" || kind == "fork" && value.Draft.MembershipSealed {
		return redaction.Receipt{}, nil, false, ErrInvalidProduction
	}
	var head int64
	if err := tx.QueryRowContext(ctx, `SELECT head_revision FROM production_sets WHERE id=?`, setID).Scan(&head); err != nil || head < value.Receipt.Revision {
		return redaction.Receipt{}, nil, false, ErrInvalidProduction
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM production_revisions WHERE set_id=? AND revision=?)`, setID, value.Receipt.Revision).Scan(&exists); err != nil || exists != 1 {
		return redaction.Receipt{}, nil, false, ErrInvalidProduction
	}
	if err := validateProductionOperationAuditTx(ctx, tx, operationID, setID, value.Receipt.Revision, storedActor, kind, storedSHA256, raw); err != nil {
		return redaction.Receipt{}, nil, false, err
	}
	return value.Receipt, value.Draft, true, nil
}

func validateProductionOperationAuditTx(ctx context.Context, tx *sql.Tx, operationID, setID string, revision int64, actor, kind, requestSHA256 string, receiptJSON []byte) error {
	var auditSetID, auditActor, auditKind, auditRequestSHA256, auditReceiptSHA256 string
	var auditRevision int64
	if err := tx.QueryRowContext(ctx, `SELECT set_id,revision,actor,kind,request_sha256,receipt_sha256
		FROM production_audit_evidence WHERE operation_id=?`, operationID).
		Scan(&auditSetID, &auditRevision, &auditActor, &auditKind, &auditRequestSHA256, &auditReceiptSHA256); err != nil ||
		auditSetID != setID || auditRevision != revision || auditActor != actor || auditKind != kind ||
		auditRequestSHA256 != requestSHA256 || auditReceiptSHA256 != productionSHA256(receiptJSON) {
		return ErrInvalidProduction
	}
	return nil
}

func recordProductionMutationTx(ctx context.Context, tx *sql.Tx, actor, kind string, receipt redaction.Receipt, draft *redaction.Draft) error {
	if !validProductionActor(actor) || redaction.ValidateReceipt(receipt) != nil || draft != nil && redaction.ValidateDraft(*draft) != nil {
		return ErrInvalidProduction
	}
	value := productionMutationReceiptV1{Version: 1, Kind: kind, Receipt: receipt, Draft: draft}
	raw, err := canonical.Marshal(value)
	if err != nil || len(raw) > redaction.MaxCommandBytes {
		return errors.Join(ErrInvalidProduction, err)
	}
	createdAt := nowRFC3339()
	if _, err := tx.ExecContext(ctx, `INSERT INTO production_operations(operation_id,set_id,actor,kind,request_sha256,receipt_json,created_at) VALUES(?,?,?,?,?,?,?)`,
		receipt.OperationID, receipt.SetID, actor, kind, receipt.RequestSHA256, raw, createdAt); err != nil {
		return fmt.Errorf("recording production operation: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO production_audit_evidence(operation_id,set_id,revision,actor,kind,request_sha256,receipt_sha256,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		receipt.OperationID, receipt.SetID, receipt.Revision, actor, kind, receipt.RequestSHA256, productionSHA256(raw), createdAt); err != nil {
		return fmt.Errorf("recording production audit evidence: %w", err)
	}
	return nil
}
