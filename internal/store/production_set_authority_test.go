package store

import (
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	productionservice "go.kenn.io/docbank/internal/production"
)

func TestProductionGateReadersPreserveCloseErrors(t *testing.T) {
	closeErr := errors.New("synthetic rows close failure")
	require.ErrorIs(t, productionRowsResult(nil, closeErr), closeErr)
	iterationErr := errors.New("synthetic rows iteration failure")
	joined := productionRowsResult(iterationErr, closeErr)
	require.ErrorIs(t, joined, iterationErr)
	require.ErrorIs(t, joined, closeErr)
}

func TestProductionTextMapExactRetryAndConflict(t *testing.T) {
	seedProductionGateAuthority(t)
}

func TestProductionCreateRetries(t *testing.T) {
	s := newTestStore(t)
	request := redaction.CreateRequest{
		OperationID:  "11111111-1111-4111-8111-111111111111",
		Name:         "Synthetic review",
		Instructions: "Keep the synthetic budget discussion.",
	}
	created, draft, err := s.CreateProductionSet(t.Context(), "test-agent", request)
	require.NoError(t, err)
	generic, err := documentproduction.GenericPolicyVersion()
	require.NoError(t, err)
	require.Equal(t, redaction.PolicySelection{
		PolicyID: generic.ID, Version: generic.Version, PolicySHA256: generic.SHA256,
	}, draft.Policy)

	var policyDigest, numberingDigest string
	require.NoError(t, s.db.QueryRow(`SELECT sha256 FROM production_policy_versions
		WHERE policy_id=? AND version=?`, generic.ID, generic.Version).Scan(&policyDigest))
	require.Equal(t, generic.SHA256, policyDigest)
	require.NoError(t, s.db.QueryRow(`SELECT sha256 FROM production_catalog_entries
		WHERE kind='numbering' AND id=''`).Scan(&numberingDigest))
	require.Equal(t, draft.NumberingRecipeSHA256, numberingDigest)

	var revisionPolicyID, revisionPolicyDigest, numberingKind, numberingID string
	var revisionPolicyVersion int64
	require.NoError(t, s.db.QueryRow(`SELECT policy_id,policy_version,policy_sha256,
		numbering_recipe_kind,numbering_recipe_id FROM production_revisions
		WHERE set_id=? AND revision=?`, created.ID, draft.Revision).Scan(
		&revisionPolicyID, &revisionPolicyVersion, &revisionPolicyDigest, &numberingKind, &numberingID))
	require.Equal(t, draft.Policy.PolicyID, revisionPolicyID)
	require.Equal(t, draft.Policy.Version, revisionPolicyVersion)
	require.Equal(t, draft.Policy.PolicySHA256, revisionPolicyDigest)
	require.Equal(t, "numbering", numberingKind)
	require.Empty(t, numberingID)

	var recipeJSON []byte
	require.NoError(t, s.db.QueryRow(`SELECT canonical_json FROM production_catalog_entries
		WHERE kind='recipe' AND id=?`, redaction.RecipeID300DPI).Scan(&recipeJSON))
	recipe, err := canonical.Decode[redaction.Recipe](recipeJSON)
	require.NoError(t, err)
	recipe.DPI = 600
	swappedRecipeJSON, err := canonical.Marshal(recipe)
	require.NoError(t, err)
	require.ErrorIs(t, validateProductionCatalogEntry("recipe", redaction.RecipeID300DPI,
		productionSHA256(swappedRecipeJSON), swappedRecipeJSON), ErrInvalidProduction)

	replayed, _, err := s.CreateProductionSet(t.Context(), "test-agent", request)
	require.NoError(t, err)
	require.Equal(t, created.ID, replayed.ID)

	request.Instructions = "Keep every synthetic discussion."
	_, _, err = s.CreateProductionSet(t.Context(), "test-agent", request)
	require.ErrorIs(t, err, ErrProductionOperationConflict)
}

func TestProductionGateLoadsStoredDerivedAuthority(t *testing.T) {
	s, member := seedProductionGateAuthority(t)
	first, second := member, member
	first.ID, first.Ordinal = "70000000-0000-4000-8000-000000000001", 1
	second.ID, second.Ordinal = "70000000-0000-4000-8000-000000000002", 2

	set, draft, err := s.CreateProductionSet(t.Context(), "test-agent", redaction.CreateRequest{
		OperationID: "70000000-0000-4000-8000-000000000003", Name: "Duplicate synthetic occurrences",
		Instructions: "Keep the selected synthetic character.",
	})
	require.NoError(t, err)
	added, err := s.ApplyProductionChanges(t.Context(), "test-agent", set.ID, draft.Revision, redaction.ApplyRequest{
		OperationID: "70000000-0000-4000-8000-000000000004", ETag: draft.ETag,
		Changes: []redaction.Change{{Kind: "member", Member: &first}, {Kind: "member", Member: &second}},
	})
	require.NoError(t, err)
	firstDecision := productionAuthorityDecision(first, "70000000-0000-4000-8000-000000000005")
	secondDecision := productionAuthorityDecision(second, "70000000-0000-4000-8000-000000000006")
	decided, err := s.ApplyProductionChanges(t.Context(), "test-agent", set.ID, draft.Revision, redaction.ApplyRequest{
		OperationID: "70000000-0000-4000-8000-000000000007", ETag: added.ETag,
		Changes: []redaction.Change{{Kind: "decision", Decision: &firstDecision}, {Kind: "decision", Decision: &secondDecision}},
	})
	require.NoError(t, err)
	current, err := s.ProductionDraft(t.Context(), set.ID, draft.Revision)
	require.NoError(t, err)
	sealed, err := s.SealProductionMembership(t.Context(), "test-agent", set.ID, draft.Revision, redaction.MembershipSealRequest{
		OperationID: "70000000-0000-4000-8000-000000000008", ETag: decided.ETag,
		Total: 2, MemberHash: current.MemberHash,
	})
	require.NoError(t, err)

	for index, memberID := range []string{first.ID, second.ID} {
		stored := loadProductionInputsForTest(t, s, set.ID, draft.Revision)
		binding := productionReviewBindingForTest(t, stored, memberID)
		reviewed, reviewErr := s.ReviewProductionMember(t.Context(), "test-agent", set.ID, draft.Revision, ProductionReviewRequest{
			OperationID: []string{"70000000-0000-4000-8000-000000000009", "70000000-0000-4000-8000-000000000010"}[index],
			ETag:        sealed.ETag + int64(index), MemberID: memberID, Binding: binding, Complete: true,
		})
		require.NoError(t, reviewErr)
		require.Equal(t, sealed.ETag+int64(index)+1, reviewed.ETag)
	}

	page, next, err := s.ProductionMembers(t.Context(), set.ID, draft.Revision, "", 1)
	require.NoError(t, err)
	require.Len(t, page, 1)
	require.NotEmpty(t, next)
	last, end, err := s.ProductionMembers(t.Context(), set.ID, draft.Revision, next, 1)
	require.NoError(t, err)
	require.Len(t, last, 1)
	require.Empty(t, end)
	require.Equal(t, []string{first.ID, second.ID}, []string{page[0].ID, last[0].ID})
	decisionPage, decisionNext, err := s.ProductionDecisions(t.Context(), set.ID, draft.Revision, "", 1)
	require.NoError(t, err)
	require.Len(t, decisionPage, 1)
	require.NotEmpty(t, decisionNext)
	decisionLast, decisionEnd, err := s.ProductionDecisions(t.Context(), set.ID, draft.Revision, decisionNext, 1)
	require.NoError(t, err)
	require.Len(t, decisionLast, 1)
	require.Empty(t, decisionEnd)
	require.Equal(t, []string{firstDecision.ID, secondDecision.ID}, []string{decisionPage[0].ID, decisionLast[0].ID})

	stored := loadProductionInputsForTest(t, s, set.ID, draft.Revision)
	require.Equal(t, map[string][]string{"family.kind": {"standalone"}}, stored.Members[0].Facts.Fields)
	require.Equal(t, []string{"synthetic-label"}, stored.Members[0].Facts.Labels)
	require.True(t, stored.Members[0].Facts.FamilyComplete)
	revisionSHA256, err := productionservice.ProductionRevisionSHA256(stored)
	require.NoError(t, err)
	gateStore, err := NewProductionGateStore(s, s.LoadProductionGateSnapshot)
	require.NoError(t, err)
	request := productionservice.PreparedInputRequest{
		OperationID: "70000000-0000-4000-8000-000000000011",
		ReceiptID:   "70000000-0000-4000-8000-000000000012",
		SetID:       set.ID, Revision: draft.Revision, ExpectedETag: stored.Draft.ETag,
		ExpectedRevisionSHA256: revisionSHA256,
		PreparedAt:             time.Date(2026, time.September, 22, 23, 0, 0, 0, time.UTC),
	}
	authority, err := productionservice.RunPreparedInputGates(t.Context(), gateStore, request)
	require.NoErrorf(t, err, "gate error: %#v; results: %#v", err, authority.GateResults)
	require.NotNil(t, authority.Receipt)
	require.Len(t, authority.Prepared.Members, 2)
	require.Equal(t, authority.Prepared.Members[0].Member.SourceVersionID, authority.Prepared.Members[1].Member.SourceVersionID)
	require.NotEqual(t, authority.Prepared.Members[0].Member.ID, authority.Prepared.Members[1].Member.ID)
	require.Equal(t, member.PDFSHA256, authority.Prepared.Members[0].Member.PDFSHA256)
	modeChanged, err := s.ApplyProductionChanges(t.Context(), "test-agent", set.ID, draft.Revision, redaction.ApplyRequest{
		OperationID: "70000000-0000-4000-8000-000000000016", ETag: stored.Draft.ETag,
		Changes: []redaction.Change{{Kind: "mode", MemberID: first.ID, Mode: "redact_selected"}},
	})
	require.NoError(t, err)
	require.Equal(t, stored.Draft.ETag+1, modeChanged.ETag)
	afterMode, _, err := s.ProductionMembers(t.Context(), set.ID, draft.Revision, "", 200)
	require.NoError(t, err)
	require.Len(t, afterMode, 2)
	require.False(t, afterMode[0].Reviewed, "a changed global member hash invalidates the edited occurrence")
	require.False(t, afterMode[1].Reviewed, "a changed global member hash invalidates every occurrence binding")
	fork, err := s.ForkProductionDraft(t.Context(), "test-agent", set.ID, draft.Revision,
		"70000000-0000-4000-8000-000000000015")
	require.NoError(t, err)
	require.Equal(t, int64(2), fork.Revision)
	require.Equal(t, draft.Policy, fork.Policy)
	forkedMembers, _, err := s.ProductionMembers(t.Context(), set.ID, fork.Revision, "", 200)
	require.NoError(t, err)
	require.Len(t, forkedMembers, 2)
	require.False(t, forkedMembers[0].Reviewed, "forked review declarations must be invalidated")
	require.False(t, forkedMembers[1].Reviewed, "forked review declarations must be invalidated")
	_, _, err = s.ProductionMembers(t.Context(), set.ID, draft.Revision, "", redaction.MaxProductionPage+1)
	require.ErrorIs(t, err, ErrInvalidProduction)
	_, _, err = s.ProductionDecisions(t.Context(), set.ID, draft.Revision, "", redaction.MaxProductionDecisionPage)
	require.NoError(t, err)
	_, _, err = s.ProductionDecisions(t.Context(), set.ID, draft.Revision, "", redaction.MaxProductionDecisionPage+1)
	require.ErrorIs(t, err, ErrInvalidProduction)

	changed := page[0]
	changed.SourceSHA256 = testSHA256([]byte("substituted source"))
	changedJSON, err := canonical.Marshal(changed)
	require.NoError(t, err)
	_, err = s.db.Exec(`UPDATE production_members SET canonical_json=? WHERE set_id=? AND revision=? AND member_id=?`,
		changedJSON, set.ID, draft.Revision, changed.ID)
	require.NoError(t, err)
	tamperedOperationID := "70000000-0000-4000-8000-000000000013"
	request.OperationID = tamperedOperationID
	request.ReceiptID = "70000000-0000-4000-8000-000000000014"
	_, err = productionservice.RunPreparedInputGates(t.Context(), gateStore, request)
	require.ErrorIs(t, err, ErrInvalidProduction)
	var retained int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM production_operation_receipts WHERE operation_id=?`, tamperedOperationID).Scan(&retained))
	require.Zero(t, retained, "source substitution must fail before a gate receipt is retained")
}

func TestProductionConfiguredPolicyUsesOnlyStoredFactsAndWithheldBinding(t *testing.T) {
	s, member := seedProductionGateAuthority(t)
	member.ID, member.Ordinal = "73000000-0000-4000-8000-000000000001", 1
	member.Family.Kind = "email_message"
	policyValue := documentproduction.PolicyVersion{
		Contract: documentproduction.PolicyContractV1, ID: "73000000-0000-4000-8000-000000000002", Version: 1,
		Name: "Synthetic configured scope", CreatedAt: "2026-09-23T00:00:00Z",
		Rules: []documentproduction.PolicyRule{{
			ID: "configured-scope", Kind: documentproduction.PolicyRuleScope,
			Predicate:   documentproduction.PolicyPredicate{Field: "document.scope", Operator: documentproduction.PolicyOperatorEquals, Values: []string{"selected"}},
			Disposition: documentproduction.PolicyDispositionProduce,
		}},
		ConflictMode: documentproduction.PolicyConflictReject,
	}
	preparedPolicy, err := productionservice.PreparePolicyVersion("73000000-0000-4000-8000-000000000003", policyValue)
	require.NoError(t, err)
	policy, err := s.PutProductionPolicy(t.Context(), preparedPolicy)
	require.NoError(t, err)
	set, draft, err := s.CreateProductionSet(t.Context(), "test-agent", redaction.CreateRequest{
		OperationID: "73000000-0000-4000-8000-000000000004", Name: "Configured synthetic policy",
		Instructions: "Use only retained policy facts.", PolicyID: policy.ID, PolicyVersion: policy.Version,
	})
	require.NoError(t, err)
	decision := productionAuthorityDecision(member, "73000000-0000-4000-8000-000000000005")
	_, err = s.ApplyProductionChanges(t.Context(), "test-agent", set.ID, draft.Revision, redaction.ApplyRequest{
		OperationID: "73000000-0000-4000-8000-000000000006", ETag: draft.ETag,
		Changes: []redaction.Change{{Kind: "member", Member: &member}, {Kind: "decision", Decision: &decision}},
	})
	require.NoError(t, err)
	selectionValue := documentproduction.WithheldSelection{
		Contract: documentproduction.WithheldSelectionContractV1, ID: "73000000-0000-4000-8000-000000000007",
		SetID: set.ID, Revision: draft.Revision, PolicySHA256: policy.SHA256,
		Members: []documentproduction.WithheldMember{{
			ID: member.ID, Ordinal: member.Ordinal, SourceVersionID: member.SourceVersionID,
			SourceSHA256: member.SourceSHA256, SourceSize: member.SourceSize, FamilyOrder: 1, Family: member.Family,
		}},
	}
	preparedSelection, err := productionservice.PrepareWithheldSelection(
		"73000000-0000-4000-8000-000000000008", selectionValue, nil)
	require.NoError(t, err)
	selection, err := s.PutProductionWithheldSelection(t.Context(), preparedSelection)
	require.NoError(t, err)

	stored := loadProductionInputsForTest(t, s, set.ID, draft.Revision)
	require.Equal(t, policy.SHA256, stored.Policy.SHA256)
	require.NotNil(t, stored.Withheld)
	require.Equal(t, selection, *stored.Withheld)
	require.Equal(t, map[string][]string{"family.kind": {"email_message"}}, stored.Members[0].Facts.Fields)
	require.False(t, stored.Members[0].Facts.FamilyComplete,
		"email completeness cannot be asserted without a root publication selector")
	require.NotContains(t, stored.Members[0].Facts.Fields, "document.scope")
	evaluation, err := documentproduction.EvaluatePolicy(stored.Policy,
		[]documentproduction.PolicyMemberFacts{stored.Members[0].Facts})
	require.NoError(t, err)
	require.Empty(t, evaluation.Members[0].Disposition,
		"storage must not invent configured policy fields from unrelated metadata")
}

func TestProductionConcurrentETagHasOneWinnerAndReplay(t *testing.T) {
	s := newTestStore(t)
	set, draft, err := s.CreateProductionSet(t.Context(), "test-agent", redaction.CreateRequest{
		OperationID: "72000000-0000-4000-8000-000000000001", Name: "Synthetic ETag race",
		Instructions: "Keep the cited synthetic text.",
	})
	require.NoError(t, err)
	requests := []redaction.ApplyRequest{
		{OperationID: "72000000-0000-4000-8000-000000000002", ETag: draft.ETag,
			Changes: []redaction.Change{{Kind: "recipe", RecipeID: redaction.RecipeID300DPI}}},
		{OperationID: "72000000-0000-4000-8000-000000000003", ETag: draft.ETag,
			Changes: []redaction.Change{{Kind: "recipe", RecipeID: redaction.RecipeID600DPI}}},
	}
	type result struct {
		index   int
		receipt redaction.Receipt
		err     error
	}
	results := make(chan result, len(requests))
	var start sync.WaitGroup
	start.Add(1)
	for index := range requests {
		go func(index int) {
			start.Wait()
			receipt, applyErr := s.ApplyProductionChanges(t.Context(), "test-agent", set.ID, draft.Revision, requests[index])
			results <- result{index: index, receipt: receipt, err: applyErr}
		}(index)
	}
	start.Done()
	winner := -1
	for range requests {
		got := <-results
		if got.err == nil {
			require.Equal(t, -1, winner)
			winner = got.index
			require.Equal(t, int64(2), got.receipt.ETag)
		} else {
			require.ErrorIs(t, got.err, ErrProductionRevisionConflict)
		}
	}
	require.NotEqual(t, -1, winner)

	_, err = s.EditProductionInstructions(t.Context(), "test-agent", set.ID, draft.Revision, redaction.InstructionsEditRequest{
		OperationID: "72000000-0000-4000-8000-000000000004", ETag: 2,
		Instructions: "Keep only the cited synthetic text.",
	})
	require.NoError(t, err)
	replayed, err := s.ApplyProductionChanges(t.Context(), "restored-caller", set.ID, draft.Revision, requests[winner])
	require.NoError(t, err)
	require.Equal(t, int64(2), replayed.ETag)

	changed := requests[winner]
	changed.Changes = append([]redaction.Change(nil), changed.Changes...)
	changed.Changes[0].RecipeID = map[string]string{
		redaction.RecipeID300DPI: redaction.RecipeID600DPI,
		redaction.RecipeID600DPI: redaction.RecipeID300DPI,
	}[changed.Changes[0].RecipeID]
	_, err = s.ApplyProductionChanges(t.Context(), "test-agent", set.ID, draft.Revision, changed)
	require.ErrorIs(t, err, ErrProductionOperationConflict)
}

func TestProductionReviewInvalidatesOnInstructionChange(t *testing.T) {
	s, member := seedProductionGateAuthority(t)
	member.ID, member.Ordinal = "71000000-0000-4000-8000-000000000001", 1
	set, draft, err := s.CreateProductionSet(t.Context(), "test-agent", redaction.CreateRequest{
		OperationID: "71000000-0000-4000-8000-000000000002", Name: "Synthetic review invalidation",
		Instructions: "Keep the selected character.",
	})
	require.NoError(t, err)
	decision := productionAuthorityDecision(member, "71000000-0000-4000-8000-000000000003")
	applied, err := s.ApplyProductionChanges(t.Context(), "test-agent", set.ID, draft.Revision, redaction.ApplyRequest{
		OperationID: "71000000-0000-4000-8000-000000000004", ETag: draft.ETag,
		Changes: []redaction.Change{{Kind: "member", Member: &member}, {Kind: "decision", Decision: &decision}},
	})
	require.NoError(t, err)
	current, err := s.ProductionDraft(t.Context(), set.ID, draft.Revision)
	require.NoError(t, err)
	sealed, err := s.SealProductionMembership(t.Context(), "test-agent", set.ID, draft.Revision, redaction.MembershipSealRequest{
		OperationID: "71000000-0000-4000-8000-000000000005", ETag: applied.ETag,
		Total: 1, MemberHash: current.MemberHash,
	})
	require.NoError(t, err)
	binding := productionReviewBindingForTest(t, loadProductionInputsForTest(t, s, set.ID, draft.Revision), member.ID)
	reviewed, err := s.ReviewProductionMember(t.Context(), "test-agent", set.ID, draft.Revision, ProductionReviewRequest{
		OperationID: "71000000-0000-4000-8000-000000000006", ETag: sealed.ETag,
		MemberID: member.ID, Binding: binding, Complete: true,
	})
	require.NoError(t, err)

	_, err = s.EditProductionInstructions(t.Context(), "test-agent", set.ID, draft.Revision, redaction.InstructionsEditRequest{
		OperationID: "71000000-0000-4000-8000-000000000007", ETag: reviewed.ETag,
		Instructions: "Keep the selected synthetic character only.",
	})
	require.NoError(t, err)
	members, _, err := s.ProductionMembers(t.Context(), set.ID, draft.Revision, "", 200)
	require.NoError(t, err)
	require.Len(t, members, 1)
	require.False(t, members[0].Reviewed)
	require.Empty(t, members[0].ReviewBinding)
}

func seedProductionGateAuthority(t *testing.T) (*Store, redaction.Member) {
	t.Helper()
	return seedProductionGateAuthorityWithPDF(t, []byte("synthetic derived PDF"),
		redaction.Box{X0: 1, Y0: 1, X1: 2, Y1: 2})
}

func seedProductionGateAuthorityWithPDF(t *testing.T, pdfBytes []byte, atomBox redaction.Box) (*Store, redaction.Member) {
	t.Helper()
	s := newTestStore(t)
	email := newEmailFixture(t, s, "synthetic-production.eml")
	retained, err := s.PublishEmailGeneration(t.Context(), email.publication)
	require.NoError(t, err)
	versionID := retained.Version.ID
	nodeID, nodeRevision := retained.Version.NodeID, int64(1)
	require.NoError(t, s.db.QueryRow(`SELECT revision FROM nodes WHERE id=?`, nodeID).Scan(&nodeRevision))
	source := document.PageSource{VersionID: versionID, SHA256: retained.Version.BlobHash, Size: retained.Version.Size}
	bodyPath, body := emailSelectedBody(retained.Evidence)
	require.NotNil(t, bodyPath)
	require.NotNil(t, body)

	pdfSHA256 := testSHA256(pdfBytes)
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		return s.EnsureBlobTx(tx, pdfSHA256, int64(len(pdfBytes)))
	}))
	recipe := document.EmailPDFRecipeV1{
		Contract: document.EmailPDFContract, RendererVersion: "synthetic-renderer-v1",
		RendererSHA256: testSHA256([]byte("renderer")), WorkerSHA256: testSHA256([]byte("worker")),
		BubblewrapSHA256: testSHA256([]byte("bubblewrap")), FontsSHA256: testSHA256([]byte("fonts")), Paper: "Letter",
	}
	binding := document.EmailPDFBindingV1{
		GenerationID: retained.Generation.ID, GenerationChecksum: retained.Generation.Checksum, Recipe: recipe,
	}
	profileValue, err := document.EmailPDFProfile(binding)
	require.NoError(t, err)
	profileJSON, profileFingerprints, err := document.CanonicalProfile(profileValue)
	require.NoError(t, err)
	profile := ProcessingProfileRecord{
		Fingerprint: profileFingerprints.Profile, CanonicalProfile: jsontext.Value(profileJSON),
		RenditionRequestFingerprint:    profileFingerprints.RenditionRequest,
		EvidenceLexicalFingerprint:     profileFingerprints.EvidenceLexical,
		RetentionDisclosureFingerprint: profileFingerprints.RetentionDisclosure,
		AttachmentPolicyFingerprint:    profileValue.RetentionDisclosure.AttachmentPolicyFingerprint,
		ConsentFingerprint:             profileValue.RetentionDisclosure.ConsentFingerprint,
		RenditionDisclosureFingerprint: profileValue.Rendition.DisclosureFingerprint,
		TrustBoundary:                  profileValue.RetentionDisclosure.TrustBoundary,
	}
	policy := jsontext.Value(`{"roles":[{"max_count":1,"min_count":1,"role":"email_pdf"}],"version":1}`)
	normalizedPolicy, err := normalizeCapturedArtifactPolicyV1(policy)
	require.NoError(t, err)
	pdfArtifactID := "artifact_" + testSHA256([]byte("production PDF artifact"))
	output := document.EmailPDFOutputV1{
		BodyPath: *bodyPath, BodySHA256: body.SHA256, BodySize: body.Size,
		PDFSHA256: pdfSHA256, PDFSize: int64(len(pdfBytes)), Pages: 1,
	}
	receiptJSON, err := json.Marshal(document.RenditionReceipt{
		EmailPDF: &output, ProviderID: document.EmailPDFContract,
		RenditionRequestFingerprint: profile.RenditionRequestFingerprint,
		SourceSHA256:                source.SHA256, OperationID: "synthetic-email-pdf-operation",
		StartedAt: "2026-09-22T21:59:00Z", CompletedAt: "2026-09-22T22:00:00Z",
	}, json.Deterministic(true))
	require.NoError(t, err)
	build := RenditionBuildRecord{
		ID: testSHA256([]byte("production PDF build")), VaultID: s.VaultID(), SourceSHA256: source.SHA256,
		RenditionRequestFingerprint:       profile.RenditionRequestFingerprint,
		EvidenceLexicalFingerprint:        profile.EvidenceLexicalFingerprint,
		CapturedArtifactPolicyFingerprint: digestCatalogJSON(normalizedPolicy.canonical),
		CapturedArtifactPolicy:            normalizedPolicy.canonical,
		AuthorizationChecksum:             testSHA256([]byte("production authorization")),
		ProviderOperationID:               "synthetic-email-pdf-operation", ProviderReceipt: jsontext.Value(receiptJSON),
		EvidenceChecksum: retained.Generation.Checksum, RenditionChecksum: pdfSHA256,
		MarkdownChecksum: testSHA256([]byte("no markdown")), Completeness: document.EvidenceDegradedProvenance,
		Warnings: []string{}, CompletedAt: "2026-09-22T22:00:00.000000000Z", DeclaredArtifactCount: 1,
		Artifacts: []RenditionArtifactRecord{{
			ID: pdfArtifactID, Role: string(document.EvidenceArtifactPDF), BlobHash: pdfSHA256,
			Size: int64(len(pdfBytes)), Checksum: pdfSHA256, State: RenditionArtifactVerified,
		}}, Units: []RenditionUnitRecord{}, LexicalSegments: []RenditionLexicalSegmentRecord{},
	}
	require.NoError(t, s.StageRenditionBuild(t.Context(), build))
	attachment := RenditionAttachmentRecord{
		ID: RenditionAttachmentID(build.ID, versionID, profile.Fingerprint), VaultID: s.VaultID(), ContentVersionID: versionID,
		BuildID: build.ID, Profile: profile, AttachedAt: "2026-09-22T22:00:00.000000000Z",
	}
	require.NoError(t, publishRenditionForTest(t, s, attachment,
		"2026-09-22T22:01:00.000000000Z", testSHA256([]byte("production PDF generation"))))

	job, err := s.QueuePageJob(t.Context(), uuid.NewString(), PageJobRequest{
		NodeID: nodeID, Revision: nodeRevision, Source: source, Pages: []int{1}, RuntimeFingerprint: testSHA256([]byte("production runtime")),
	})
	require.NoError(t, err)
	claim, err := s.ClaimPageJob(t.Context())
	require.NoError(t, err)
	require.Equal(t, job.ID, claim.Job.ID)
	frame, err := document.NewPDFPageFrame(source, 1, [4]float64{0, 0, 72, 72}, [4]float64{0, 0, 72, 72}, 0)
	require.NoError(t, err)
	require.NoError(t, s.PublishPageFrames(t.Context(), claim, []document.PageFrameV1{frame}))
	_, frameSHA256, err := document.MarshalPageFrameV1(frame)
	require.NoError(t, err)
	textMap := redaction.NormalizeTextMap(redaction.TextMap{
		Contract: "aligned-text/v1", PDFSHA256: pdfSHA256, EvidenceSHA256: build.EvidenceChecksum, Text: "x",
		Pages: []redaction.Page{{Number: 1, FrameSHA256: frameSHA256, Width: frame.Width, Height: frame.Height, Span: redaction.Span{Start: 0, End: 1}}},
		Atoms: []redaction.Atom{{Span: redaction.Span{Start: 0, End: 1}, Boxes: []redaction.Box{{Page: 1, FrameSHA256: frameSHA256,
			X0: atomBox.X0, Y0: atomBox.Y0, X1: atomBox.X1, Y1: atomBox.Y1}}}},
	})
	authority := ProductionTextMapAuthority{
		SourceNodeID: nodeID, Source: source, PDFSHA256: pdfSHA256, PDFSize: int64(len(pdfBytes)),
		RenditionAttachmentID: attachment.ID, RenditionBuildID: build.ID, RenditionArtifactID: pdfArtifactID, Map: textMap,
	}
	mapSHA256, inventorySHA256, err := s.RetainProductionTextMap(t.Context(), authority)
	require.NoError(t, err)
	retryMapSHA256, retryInventorySHA256, err := s.RetainProductionTextMap(t.Context(), authority)
	require.NoError(t, err)
	require.Equal(t, mapSHA256, retryMapSHA256)
	require.Equal(t, inventorySHA256, retryInventorySHA256)
	conflict := authority
	conflict.PDFSize++
	_, _, err = s.RetainProductionTextMap(t.Context(), conflict)
	require.ErrorIs(t, err, ErrInvalidProduction)
	return s, redaction.Member{
		VaultID: s.VaultID(), SourceVersionID: versionID, SourceSHA256: source.SHA256, PDFSHA256: pdfSHA256,
		NodeID: nodeID, SourceSize: source.Size, PDFSize: int64(len(pdfBytes)),
		Family: redaction.FamilyContext{Kind: "standalone", RootVersionID: versionID}, Mode: "keep_selected",
		MapSHA256: mapSHA256, PageInventorySHA256: inventorySHA256,
	}
}

func productionAuthorityDecision(member redaction.Member, id string) redaction.Decision {
	return redaction.Decision{
		ID: id, MemberID: member.ID, Action: "keep", Reason: "synthetic relevance", Label: "synthetic-label",
		Selector: redaction.Selector{Kind: "text", MapSHA256: member.MapSHA256, Span: &redaction.Span{Start: 0, End: 1}},
	}
}

func loadProductionInputsForTest(t *testing.T, s *Store, setID string, revision int64) productionservice.StoredProductionInputs {
	t.Helper()
	var stored productionservice.StoredProductionInputs
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		var err error
		stored, err = s.loadProductionInputsTx(t.Context(), tx, setID, revision)
		return err
	}))
	return stored
}

func productionReviewBindingForTest(t *testing.T, stored productionservice.StoredProductionInputs, memberID string) string {
	t.Helper()
	for _, member := range stored.Members {
		if member.Member.ID != memberID {
			continue
		}
		_, decisionsSHA256, err := redaction.CanonicalDecisions(member.Decisions)
		require.NoError(t, err)
		_, resolvedSHA256, err := redaction.CanonicalResolved(member.Resolved)
		require.NoError(t, err)
		binding, err := redaction.ReviewBinding(redaction.ReviewInput{
			SetID: stored.Draft.SetID, MemberID: member.Member.ID, VaultID: member.Member.VaultID,
			SourceVersionID: member.Member.SourceVersionID, Revision: stored.Draft.Revision,
			Ordinal: member.Member.Ordinal, NodeID: member.Member.NodeID, SourceSize: member.Member.SourceSize,
			PDFSize: member.Member.PDFSize, SourceSHA256: member.Member.SourceSHA256, PDFSHA256: member.Member.PDFSHA256,
			PageInventorySHA256: member.Member.PageInventorySHA256, MapSHA256: member.Member.MapSHA256,
			Mode: member.Member.Mode, MemberHash: stored.Draft.MemberHash, InstructionsSHA256: stored.Draft.InstructionsSHA256,
			RecipeSHA256: stored.Draft.RecipeSHA256, DecisionsSHA256: decisionsSHA256, ResolvedSHA256: resolvedSHA256,
		})
		require.NoError(t, err)
		return binding
	}
	t.Fatalf("member %s not found", memberID)
	return ""
}
