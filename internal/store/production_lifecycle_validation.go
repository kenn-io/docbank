package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	productionservice "go.kenn.io/docbank/internal/production"
)

// validateProductionLifecycleState runs against the pinned export view and the
// uncommitted import transaction. The manifest protects missing rows; these
// checks protect relationships even when a caller recomputes all checksums.
func validateProductionLifecycleState(ctx context.Context, q metadataQuerier) error {
	if err := validateProductionLifecycleMaps(ctx, q); err != nil {
		return err
	}
	if err := validateProductionLifecycleCatalog(ctx, q); err != nil {
		return err
	}
	if err := validateProductionLifecycleRevisions(ctx, q); err != nil {
		return err
	}
	if err := validateProductionLifecycleOperations(ctx, q); err != nil {
		return err
	}
	if err := validateProductionLifecycleFinalized(ctx, q); err != nil {
		return err
	}
	if err := validateProductionLifecycleJobs(ctx, q); err != nil {
		return err
	}
	if err := validateProductionLifecycleArtifactsAndPlans(ctx, q); err != nil {
		return err
	}
	return validateProductionLifecycleStages(ctx, q)
}

func validateProductionLifecycleOperations(ctx context.Context, q metadataQuerier) error {
	rows, err := q.QueryContext(ctx, `SELECT operation_id,set_id,actor,kind,request_sha256,
		receipt_json,created_at FROM production_operations ORDER BY operation_id`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var operationID, setID, actor, kind, requestSHA, createdAt string
		var raw []byte
		if err := rows.Scan(&operationID, &setID, &actor, &kind, &requestSHA, &raw, &createdAt); err != nil {
			return err
		}
		var receipt redaction.Receipt
		var encoded []byte
		if kind == "create" {
			value, err := canonical.Decode[productionCreateReceiptV1](raw)
			if err != nil || value.Version != 1 || redaction.ValidateSet(value.Set) != nil ||
				redaction.ValidateDraft(value.Draft) != nil || value.Set.ID != setID ||
				value.Draft.SetID != setID {
				return fmt.Errorf("production create operation %s contradicts stored receipt", operationID)
			}
			receipt = value.Receipt
			encoded, err = canonical.Marshal(value)
			if err != nil {
				return err
			}
		} else {
			value, err := canonical.Decode[productionMutationReceiptV1](raw)
			if err != nil || value.Version != 1 || value.Kind != kind ||
				value.Draft != nil && redaction.ValidateDraft(*value.Draft) != nil {
				return fmt.Errorf("production operation %s contradicts stored receipt", operationID)
			}
			receipt = value.Receipt
			encoded, err = canonical.Marshal(value)
			if err != nil {
				return err
			}
		}
		if redaction.ValidateReceipt(receipt) != nil ||
			receipt.OperationID != operationID || receipt.SetID != setID ||
			receipt.RequestSHA256 != requestSHA || !validProductionActor(actor) {
			return fmt.Errorf("production operation %s contradicts stored receipt", operationID)
		}
		if !bytes.Equal(encoded, raw) {
			return fmt.Errorf("production operation %s receipt is not canonical", operationID)
		}
		var auditSetID, auditActor, auditKind, auditRequestSHA, auditReceiptSHA, auditCreatedAt string
		var auditRevision int64
		if err := q.QueryRowContext(ctx, `SELECT set_id,revision,actor,kind,request_sha256,
			receipt_sha256,created_at FROM production_audit_evidence WHERE operation_id=?`, operationID).
			Scan(&auditSetID, &auditRevision, &auditActor, &auditKind, &auditRequestSHA,
				&auditReceiptSHA, &auditCreatedAt); err != nil ||
			auditSetID != setID || auditRevision != receipt.Revision ||
			auditActor != actor || auditKind != kind || auditRequestSHA != requestSHA ||
			auditReceiptSHA != digestProductionBytes(raw) || auditCreatedAt != createdAt {
			return fmt.Errorf("production operation %s has contradictory audit evidence", operationID)
		}
	}
	return rows.Err()
}

func validateProductionLifecycleArtifactsAndPlans(ctx context.Context, q metadataQuerier) error {
	artifacts, err := q.QueryContext(ctx, `SELECT job_id,artifact_id,artifact_json FROM production_job_artifacts ORDER BY job_id,artifact_id`)
	if err != nil {
		return err
	}
	defer func() { _ = artifacts.Close() }()
	for artifacts.Next() {
		var jobID, id string
		var raw []byte
		if err := artifacts.Scan(&jobID, &id, &raw); err != nil {
			_ = artifacts.Close()
			return err
		}
		artifact, err := canonical.Decode[documentproduction.Artifact](raw)
		if err != nil || artifact.ID != id {
			_ = artifacts.Close()
			return fmt.Errorf("production artifact %s/%s contradicts its identity", jobID, id)
		}
		if _, _, err := documentproduction.CanonicalArtifactManifest(documentproduction.ArtifactManifest{
			Contract:  documentproduction.ArtifactManifestContractV1,
			Artifacts: []documentproduction.Artifact{artifact},
		}); err != nil {
			_ = artifacts.Close()
			return fmt.Errorf("production artifact %s/%s is invalid: %w", jobID, id, err)
		}
		encoded, err := canonical.Marshal(artifact)
		if err != nil || !bytes.Equal(encoded, raw) {
			_ = artifacts.Close()
			return fmt.Errorf("production artifact %s/%s is not canonical", jobID, id)
		}
		var size int64
		if err := q.QueryRowContext(ctx, `SELECT size FROM blobs WHERE hash=?`, artifact.SHA256).Scan(&size); err != nil || size != artifact.Size {
			_ = artifacts.Close()
			return fmt.Errorf("production artifact %s/%s has no matching blob", jobID, id)
		}
	}
	if err := errors.Join(artifacts.Err(), artifacts.Close()); err != nil {
		return err
	}
	plans, err := q.QueryContext(ctx, `SELECT job_id,allocation_id,reservation_sha256,plan_sha256,
		canonical_json FROM production_job_render_plans ORDER BY job_id`)
	if err != nil {
		return err
	}
	defer func() { _ = plans.Close() }()
	for plans.Next() {
		var jobID, allocationID, reservationSHA, planSHA string
		var raw []byte
		if err := plans.Scan(&jobID, &allocationID, &reservationSHA, &planSHA, &raw); err != nil {
			return err
		}
		plan, err := canonical.Decode[productionservice.RenderPlan](raw)
		if err != nil {
			return fmt.Errorf("production render plan %s: %w", jobID, err)
		}
		plan, encoded, err := productionservice.CanonicalRenderPlan(plan)
		if err != nil || !bytes.Equal(encoded, raw) || plan.SHA256 != planSHA ||
			plan.JobID != jobID || plan.Reservation.ID != allocationID ||
			plan.Reservation.SHA256 != reservationSHA {
			return fmt.Errorf("production render plan %s contradicts stored authority", jobID)
		}
		var jobRevisionSHA string
		var jobAllocationID sql.NullString
		if err := q.QueryRowContext(ctx, `SELECT revision_sha256,allocation_id FROM production_jobs WHERE job_id=?`, jobID).
			Scan(&jobRevisionSHA, &jobAllocationID); err != nil || jobRevisionSHA != plan.RevisionSHA256 ||
			jobAllocationID.Valid && jobAllocationID.String != allocationID {
			return fmt.Errorf("production render plan %s contradicts job reservation", jobID)
		}
	}
	return plans.Err()
}

func validateProductionLifecycleMaps(ctx context.Context, q metadataQuerier) error {
	rows, err := q.QueryContext(ctx, `SELECT map_sha256,version_id,pdf_sha256,evidence_sha256,
		page_inventory_sha256,canonical_json FROM production_text_maps ORDER BY map_sha256`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var digest, versionID, pdfSHA, evidenceSHA, inventorySHA string
		var raw []byte
		if err := rows.Scan(&digest, &versionID, &pdfSHA, &evidenceSHA, &inventorySHA, &raw); err != nil {
			return err
		}
		value, actualSHA, err := redaction.DecodeTextMap(raw)
		if err != nil {
			return fmt.Errorf("production text map %s: %w", digest, err)
		}
		if actualSHA != digest ||
			value.PDFSHA256 != pdfSHA || value.EvidenceSHA256 != evidenceSHA {
			return fmt.Errorf("production text map %s contradicts canonical map", digest)
		}
		inventory, err := productionPageInventorySHA256(value.Pages)
		if err != nil || inventory != inventorySHA {
			return fmt.Errorf("production text map %s contradicts page inventory", digest)
		}
		frames, err := q.QueryContext(ctx, `SELECT version_id,page,frame_sha256,width,height
			FROM production_text_map_frames WHERE map_sha256=? ORDER BY page`, digest)
		if err != nil {
			return err
		}
		defer func() { _ = frames.Close() }()
		index := 0
		for frames.Next() {
			var frameVersion, frameSHA string
			var page int
			var width, height int64
			if err := frames.Scan(&frameVersion, &page, &frameSHA, &width, &height); err != nil {
				_ = frames.Close()
				return err
			}
			if index >= len(value.Pages) || frameVersion != versionID ||
				value.Pages[index].Number != page || value.Pages[index].FrameSHA256 != frameSHA ||
				value.Pages[index].Width != width || value.Pages[index].Height != height {
				_ = frames.Close()
				return fmt.Errorf("production text map %s has contradictory frame", digest)
			}
			index++
		}
		if err := errors.Join(frames.Err(), frames.Close()); err != nil {
			return err
		}
		if index != len(value.Pages) {
			return fmt.Errorf("production text map %s has missing frames", digest)
		}
	}
	return rows.Err()
}

func validateProductionLifecycleRevisions(ctx context.Context, q metadataQuerier) error {
	setRows, err := q.QueryContext(ctx, `SELECT id,name,creator,created_at,head_revision FROM production_sets ORDER BY id`)
	if err != nil {
		return err
	}
	defer func() { _ = setRows.Close() }()
	for setRows.Next() {
		if _, err := scanProductionSet(setRows); err != nil {
			_ = setRows.Close()
			return err
		}
	}
	if err := errors.Join(setRows.Err(), setRows.Close()); err != nil {
		return err
	}
	revisions, err := q.QueryContext(ctx, `SELECT set_id,revision,instructions,instructions_sha256,
		member_hash,decisions_sha256 FROM production_revisions ORDER BY set_id,revision`)
	if err != nil {
		return err
	}
	defer func() { _ = revisions.Close() }()
	for revisions.Next() {
		var setID, instructionsSHA, memberSHA, decisionsSHA string
		var revision int64
		var instructions []byte
		if err := revisions.Scan(&setID, &revision, &instructions, &instructionsSHA, &memberSHA, &decisionsSHA); err != nil {
			return err
		}
		if digestProductionBytes(instructions) != instructionsSHA {
			return fmt.Errorf("production revision %s/%d has contradictory instructions", setID, revision)
		}
		if _, err := scanProductionDraft(q.QueryRowContext(ctx, productionDraftSelect+` WHERE set_id=? AND revision=?`, setID, revision)); err != nil {
			return err
		}
		members, err := q.QueryContext(ctx, `SELECT member_id,ordinal,vault_id,node_id,version_id,
			source_sha256,source_size,pdf_sha256,pdf_size,map_sha256,page_inventory_sha256,
			family_context_json,canonical_json FROM production_members
			WHERE set_id=? AND revision=? ORDER BY ordinal,member_id`, setID, revision)
		if err != nil {
			return err
		}
		defer func() { _ = members.Close() }()
		var values []redaction.Member
		for members.Next() {
			member, err := scanProductionMember(members)
			if err != nil {
				_ = members.Close()
				return err
			}
			values = append(values, member)
		}
		if err := errors.Join(members.Err(), members.Close()); err != nil {
			return err
		}
		actualMembers, err := productionMemberHash(values)
		if err != nil || actualMembers != memberSHA {
			return fmt.Errorf("production revision %s/%d has contradictory members", setID, revision)
		}
		decisions, err := q.QueryContext(ctx, `SELECT revision,decision_id,member_id,actor,created_at,
			canonical_json FROM production_decisions WHERE set_id=? AND revision=? ORDER BY member_id,decision_id`, setID, revision)
		if err != nil {
			return err
		}
		defer func() { _ = decisions.Close() }()
		var decisionValues []redaction.Decision
		for decisions.Next() {
			decision, err := scanProductionDecision(decisions)
			if err != nil {
				_ = decisions.Close()
				return err
			}
			decisionValues = append(decisionValues, decision)
		}
		if err := errors.Join(decisions.Err(), decisions.Close()); err != nil {
			return err
		}
		actualDecisions, err := productionDecisionHash(decisionValues)
		if err != nil || actualDecisions != decisionsSHA {
			return fmt.Errorf("production revision %s/%d has contradictory decisions", setID, revision)
		}
		if len(values) != 0 {
			memberIDs := make(map[string]struct{}, len(values))
			for _, value := range values {
				memberIDs[value.ID] = struct{}{}
			}
			for _, decision := range decisionValues {
				if _, found := memberIDs[decision.MemberID]; !found {
					return fmt.Errorf("production revision %s/%d has detached decision", setID, revision)
				}
			}
		}
	}
	return revisions.Err()
}

func validateProductionLifecycleCatalog(ctx context.Context, q metadataQuerier) error {
	rows, err := q.QueryContext(ctx, `SELECT kind,id,sha256,canonical_json FROM production_catalog_entries ORDER BY kind,id`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var kind, id, digest string
		var raw []byte
		if err := rows.Scan(&kind, &id, &digest, &raw); err != nil {
			return err
		}
		if err := validateProductionCatalogEntry(kind, id, digest, raw); err != nil {
			return fmt.Errorf("production catalog %s/%s: %w", kind, id, err)
		}
	}
	return rows.Err()
}

func validateProductionLifecycleFinalized(ctx context.Context, q metadataQuerier) error {
	rows, err := q.QueryContext(ctx, `SELECT set_id,revision,etag,draft_sha256,prepared_sha256,
		prepared_input_sha256,draft_json,prepared_input_json,numbering_recipe_sha256
		FROM production_finalized_revisions ORDER BY set_id,revision`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var setID, draftSHA, preparedSHA, receiptSHA, recipeSHA string
		var revision, etag int64
		var draftRaw, authorityRaw []byte
		if err := rows.Scan(&setID, &revision, &etag, &draftSHA, &preparedSHA,
			&receiptSHA, &draftRaw, &authorityRaw, &recipeSHA); err != nil {
			return err
		}
		draft, err := canonical.Decode[redaction.Draft](draftRaw)
		if err != nil || redaction.ValidateDraft(draft) != nil || draft.State != "finalized" ||
			draft.SetID != setID || draft.Revision != revision || draft.ETag != etag ||
			digestProductionBytes(draftRaw) != draftSHA || draft.NumberingRecipeSHA256 != recipeSHA {
			return fmt.Errorf("production finalized draft %s/%d conflicts with its digest or identity", setID, revision)
		}
		encoded, err := canonical.Marshal(draft)
		if err != nil || !bytes.Equal(encoded, draftRaw) {
			return fmt.Errorf("production finalized draft %s/%d is not canonical", setID, revision)
		}
		stored, err := scanProductionDraft(q.QueryRowContext(ctx, productionDraftSelect+` WHERE set_id=? AND revision=?`, setID, revision))
		if err != nil || stored != draft {
			return fmt.Errorf("production finalized draft %s/%d differs from stored revision", setID, revision)
		}
		authority, err := canonical.Decode[documentproduction.PreparedInputAuthority](authorityRaw)
		if err != nil || documentproduction.ValidatePreparedInputAuthority(authority) != nil ||
			authority.Receipt == nil || authority.Prepared.SHA256 != preparedSHA ||
			authority.Receipt.SHA256 != receiptSHA || authority.Prepared.SetID != setID ||
			authority.Prepared.Revision != revision || authority.Prepared.ETag != etag ||
			authority.Prepared.MemberHash != draft.MemberHash ||
			authority.Prepared.DecisionsSHA256 != draft.DecisionsSHA256 {
			return fmt.Errorf("production finalized gate %s/%d conflicts with revision", setID, revision)
		}
		encoded, err = canonical.Marshal(authority)
		if err != nil || !bytes.Equal(encoded, authorityRaw) {
			return fmt.Errorf("production finalized gate %s/%d is not canonical", setID, revision)
		}
		var gateRaw []byte
		if err := q.QueryRowContext(ctx, `SELECT response_json FROM production_operation_receipts
			WHERE operation_id=? AND kind='prepared_input_gates'`, authority.Audit.OperationID).Scan(&gateRaw); err != nil || !bytes.Equal(gateRaw, authorityRaw) {
			return fmt.Errorf("production finalized gate %s/%d has no matching operation", setID, revision)
		}
	}
	return rows.Err()
}

func validateProductionLifecycleJobs(ctx context.Context, q metadataQuerier) error {
	rows, err := q.QueryContext(ctx, `SELECT job_id,operation_id,state,set_id,revision,etag,
		revision_sha256,prepared_input_sha256,receipt_sha256,request_json,allocation_id,
		receipt_json,artifact_manifest_json,endorsements_json
		FROM production_jobs ORDER BY job_id`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, operationID, state, setID, revisionSHA, gateSHA, receiptSHA string
		var revision, etag int64
		var requestRaw, receiptRaw, manifestRaw, endorsementsRaw []byte
		var allocationID *string
		if err := rows.Scan(&id, &operationID, &state, &setID, &revision, &etag,
			&revisionSHA, &gateSHA, &receiptSHA, &requestRaw, &allocationID,
			&receiptRaw, &manifestRaw, &endorsementsRaw); err != nil {
			return err
		}
		request, err := canonical.Decode[productionservice.JobRequest](requestRaw)
		if err != nil || request.JobID != id || request.OperationID != operationID ||
			request.SetID != setID || request.Revision != revision || request.ETag != etag ||
			request.RevisionSHA256 != revisionSHA || request.PreparedInputSHA256 != gateSHA {
			return fmt.Errorf("production job %s request contradicts stored identity", id)
		}
		encoded, err := canonical.Marshal(request)
		if err != nil || !bytes.Equal(encoded, requestRaw) {
			return fmt.Errorf("production job %s request is not canonical", id)
		}
		var finalizedRevisionSHA, finalizedGateSHA string
		if err := q.QueryRowContext(ctx, `SELECT prepared_sha256,prepared_input_sha256
			FROM production_finalized_revisions WHERE set_id=? AND revision=?`, setID, revision).
			Scan(&finalizedRevisionSHA, &finalizedGateSHA); err != nil ||
			finalizedRevisionSHA != revisionSHA || finalizedGateSHA != gateSHA {
			return fmt.Errorf("production job %s has no matching finalized revision", id)
		}
		switch state {
		case productionservice.ProductionJobQueued, productionservice.ProductionJobRunning,
			productionservice.ProductionJobSucceeded, productionservice.ProductionJobFailed,
			productionservice.ProductionJobCanceled:
		default:
			return fmt.Errorf("production job %s has unknown state %q", id, state)
		}
		if state == productionservice.ProductionJobSucceeded {
			if receiptSHA == "" || len(receiptRaw) == 0 || len(manifestRaw) == 0 || len(endorsementsRaw) == 0 || allocationID == nil {
				return fmt.Errorf("published production job %s lacks receipt authority", id)
			}
			receipt, err := canonical.Decode[documentproduction.ProductionReceipt](receiptRaw)
			if err != nil || documentproduction.ValidateProductionReceipt(receipt) != nil ||
				receipt.JobID != id || receipt.SetID != setID || receipt.Revision != revision ||
				receipt.RevisionSHA256 != revisionSHA || receipt.PreparedInputSHA256 != gateSHA ||
				receipt.SHA256 != receiptSHA {
				return fmt.Errorf("published production job %s has contradictory receipt", id)
			}
		} else if receiptSHA != "" || len(receiptRaw) != 0 || len(manifestRaw) != 0 || len(endorsementsRaw) != 0 {
			return fmt.Errorf("unpublished production job %s contains publication payload", id)
		}
	}
	return rows.Err()
}

func validateProductionLifecycleStages(ctx context.Context, q metadataQuerier) error {
	rows, err := q.QueryContext(ctx, `SELECT job_id,member_id,page,artifact_id,artifact_sha256,
		stage_sha256,canonical_json FROM production_job_page_stages ORDER BY job_id,member_id,page`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var jobID, memberID, artifactID, artifactSHA, stageSHA string
		var page int
		var raw []byte
		if err := rows.Scan(&jobID, &memberID, &page, &artifactID, &artifactSHA, &stageSHA, &raw); err != nil {
			return err
		}
		stage, err := canonical.Decode[productionservice.ProductionPageStage](raw)
		if err != nil {
			return fmt.Errorf("production page stage %s/%s/%d: %w", jobID, memberID, page, err)
		}
		stage, encoded, err := productionservice.CanonicalProductionPageStage(stage)
		if err != nil || !bytes.Equal(encoded, raw) || stage.SHA256 != stageSHA ||
			stage.JobID != jobID || stage.MemberID != memberID || stage.Page != page ||
			stage.Artifact.ID != artifactID || stage.Artifact.SHA256 != artifactSHA {
			return fmt.Errorf("production page stage %s/%s/%d contradicts its canonical receipt", jobID, memberID, page)
		}
		var artifactRaw []byte
		if err := q.QueryRowContext(ctx, `SELECT artifact_json FROM production_job_artifacts
			WHERE job_id=? AND artifact_id=?`, jobID, artifactID).Scan(&artifactRaw); err != nil {
			return fmt.Errorf("production page stage %s/%s/%d has no artifact: %w", jobID, memberID, page, err)
		}
		expected, err := canonical.Marshal(stage.Artifact)
		if err != nil || !bytes.Equal(expected, artifactRaw) {
			return fmt.Errorf("production page stage %s/%s/%d artifact differs from receipt", jobID, memberID, page)
		}
		var planSHA, reservationSHA string
		if err := q.QueryRowContext(ctx, `SELECT plan_sha256,reservation_sha256
			FROM production_job_render_plans WHERE job_id=?`, jobID).Scan(&planSHA, &reservationSHA); err != nil ||
			planSHA != stage.RenderPlanSHA256 || reservationSHA != stage.ReservationSHA256 {
			return fmt.Errorf("production page stage %s/%s/%d has no matching render plan", jobID, memberID, page)
		}
		var blobSize int64
		if err := q.QueryRowContext(ctx, `SELECT size FROM blobs WHERE hash=?`, artifactSHA).Scan(&blobSize); err != nil ||
			blobSize != stage.Artifact.Size {
			return fmt.Errorf("production page stage %s/%s/%d has no matching blob", jobID, memberID, page)
		}
	}
	return errors.Join(rows.Err(), rows.Close())
}
