package docbank_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestEmbeddedProductionPolicyKeepsVaultRootsSeparate(t *testing.T) {
	first, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, first.Close()) })
	second, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close()) })
	policy := documentproduction.PolicyVersion{
		Contract: documentproduction.PolicyContractV1,
		ID:       "11111111-1111-4111-8111-111111111111", Version: 1,
		Name: "Synthetic policy", CreatedAt: "2026-09-22T13:00:00Z",
		Rules: []documentproduction.PolicyRule{{
			ID: "withhold-selected", Kind: documentproduction.PolicyRuleDisposition,
			Predicate:   documentproduction.PolicyPredicate{Field: "member.id", Operator: documentproduction.PolicyOperatorPresent},
			Disposition: documentproduction.PolicyDispositionWithhold,
		}}, ConflictMode: documentproduction.PolicyConflictReject,
	}
	const operationID = "22222222-2222-4222-8222-222222222222"
	created, err := first.CreateProductionPolicyVersion(t.Context(), operationID, policy)
	require.NoError(t, err)
	require.NoError(t, documentproduction.ValidatePolicyVersion(created))
	replay, err := first.CreateProductionPolicyVersion(t.Context(), operationID, policy)
	require.NoError(t, err)
	require.Equal(t, created.SHA256, replay.SHA256)
	read, err := first.ProductionPolicyVersion(t.Context(), policy.ID, policy.Version)
	require.NoError(t, err)
	require.Equal(t, created.SHA256, read.SHA256)
	page, err := first.ProductionPolicyVersions(t.Context(), "", 1)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.Equal(t, created.SHA256, page.Items[0].SHA256)
	require.Empty(t, page.NextCursor)
	_, err = second.ProductionPolicyVersion(t.Context(), policy.ID, policy.Version)
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = first.ProductionApproval(t.Context(), "33333333-3333-4333-8333-333333333333")
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = first.ProductionPrivilegeLog(t.Context(), "44444444-4444-4444-8444-444444444444", 1, "", 1)
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestEmbeddedPrivilegeDraftMutationsUseOwnedVault(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	request := docbank.ProductionPrivilegeDraftCreateRequest{
		OperationID: "87878787-8787-4787-8787-878787878701",
		SetID:       "87878787-8787-4787-8787-878787878702", SetRevision: 1,
		PlayersSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	created, err := vault.CreateProductionPrivilegeLogDraft(t.Context(),
		"87878787-8787-4787-8787-878787878703", 1, request)
	require.ErrorIs(t, err, store.ErrNotFound)
	require.Zero(t, created)
	replaced, err := vault.ReplaceProductionPrivilegeLogRows(t.Context(),
		"87878787-8787-4787-8787-878787878703", 1,
		docbank.ProductionPrivilegeRowsReplaceRequest{
			OperationID: "87878787-8787-4787-8787-878787878704", ExpectedGeneration: 1,
			Rows: []documentproduction.PrivilegeRow{{
				ID:               "87878787-8787-4787-8787-878787878705",
				WithheldMemberID: "87878787-8787-4787-8787-878787878706",
				FamilyOrder:      1, SourceVersionID: "87878787-8787-4787-8787-878787878707",
				Basis: "synthetic_basis", PublicDescription: "Synthetic description.",
				EvidenceSHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
				PersonIDs:      []string{"87878787-8787-4787-8787-878787878708"},
			}},
		})
	require.ErrorIs(t, err, store.ErrNotFound)
	require.Zero(t, replaced)
}

func TestEmbeddedProductionSetsKeepVaultRootsSeparate(t *testing.T) {
	first, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, first.Close()) })
	second, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close()) })
	request := redaction.CreateRequest{OperationID: "88888888-8888-4888-8888-888888888888", Name: "Synthetic review"}
	set, draft, err := first.CreateProductionSet(t.Context(), "synthetic-operator", request)
	require.NoError(t, err)
	require.Equal(t, set.ID, draft.SetID)
	sets, err := first.ProductionSets(t.Context(), "", 1)
	require.NoError(t, err)
	require.Len(t, sets.Items, 1)
	require.Equal(t, set.ID, sets.Items[0].ID)
	otherSets, err := second.ProductionSets(t.Context(), "", 1)
	require.NoError(t, err)
	require.Empty(t, otherSets.Items)
	catalog, err := first.ProductionRecipes(t.Context())
	require.NoError(t, err)
	require.Equal(t, redaction.DefaultRecipeID, catalog.DefaultID)
	require.Len(t, catalog.Items, 2)
	require.Equal(t, draft.RecipeSHA256, catalog.Items[0].SHA256)
	replaySet, replayDraft, err := first.CreateProductionSet(t.Context(), "synthetic-operator", request)
	require.NoError(t, err)
	require.Equal(t, set, replaySet)
	require.Equal(t, draft, replayDraft)
	read, err := first.ProductionSet(t.Context(), set.ID)
	require.NoError(t, err)
	require.Equal(t, set, read)
	page, err := first.ProductionMembers(t.Context(), set.ID, 1, "", 1)
	require.NoError(t, err)
	require.Empty(t, page.Items)
	edit := api.ProductionInstructionsRequest{OperationID: "88888888-8888-4888-8888-888888888889",
		Instructions: "Review synthetic pages"}
	receipt, err := first.EditProductionInstructions(t.Context(), "synthetic-operator", set.ID, 1, 1, edit)
	require.NoError(t, err)
	require.EqualValues(t, 2, receipt.ETag)
	replayed, err := first.EditProductionInstructions(t.Context(), "synthetic-operator", set.ID, 1, 1, edit)
	require.NoError(t, err)
	require.Equal(t, receipt, replayed)
	_, err = first.EditProductionInstructions(t.Context(), "synthetic-operator", set.ID, 1, 1,
		api.ProductionInstructionsRequest{OperationID: "88888888-8888-4888-8888-888888888890"})
	require.ErrorIs(t, err, store.ErrProductionRevisionConflict)
	change, err := first.ApplyProductionChanges(t.Context(), "synthetic-operator", set.ID, 1, 2,
		api.ProductionChangesRequest{OperationID: "88888888-8888-4888-8888-888888888891",
			Changes: []api.ProductionChange{{Kind: "recipe", RecipeID: redaction.RecipeID600DPI}}})
	require.NoError(t, err)
	require.EqualValues(t, 3, change.ETag)
	updated, err := first.ProductionDraft(t.Context(), set.ID, 1)
	require.NoError(t, err)
	require.Equal(t, redaction.RecipeID600DPI, updated.RecipeID)
	forked, err := first.ForkProductionDraft(t.Context(), "synthetic-operator", set.ID, 1,
		"88888888-8888-4888-8888-888888888896")
	require.NoError(t, err)
	require.EqualValues(t, 2, forked.Revision)
	require.Equal(t, updated.RecipeSHA256, forked.RecipeSHA256)
	replayFork, err := first.ForkProductionDraft(t.Context(), "synthetic-operator", set.ID, 1,
		"88888888-8888-4888-8888-888888888896")
	require.NoError(t, err)
	require.Equal(t, forked, replayFork)
	_, err = second.ProductionDraft(t.Context(), set.ID, 2)
	require.ErrorIs(t, err, store.ErrNotFound)
	member := api.ProductionMember(redaction.Member{
		ID: "88888888-8888-4888-8888-888888888881", VaultID: "88888888-8888-4888-8888-888888888882",
		SourceVersionID: "88888888-8888-4888-8888-888888888883", NodeID: 1,
		SourceSHA256:        "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		PDFSHA256:           "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		MapSHA256:           "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		PageInventorySHA256: "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		PDFSize:             1, Ordinal: 1, Mode: "redact_selected",
		Family: redaction.FamilyContext{Kind: "standalone", RootVersionID: "88888888-8888-4888-8888-888888888883"},
	})
	appendRequest := api.ProductionMemberAppendRequest{OperationID: "88888888-8888-4888-8888-888888888884",
		Members: []api.ProductionMember{member}}
	_, err = second.AppendProductionMembers(t.Context(), "synthetic-operator", set.ID, 1, 1, appendRequest)
	require.ErrorIs(t, err, store.ErrProductionRevisionConflict)
	uncertain := true
	filtered, err := first.ProductionDecisionsFiltered(t.Context(), set.ID, 1, "", 500, &uncertain)
	require.NoError(t, err)
	require.Empty(t, filtered.Items)
	_, err = second.ProductionDecisionsFiltered(t.Context(), set.ID, 1, "", 1, &uncertain)
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = first.SealProductionMembership(t.Context(), "synthetic-operator", set.ID, 1, 3,
		api.ProductionMembershipSealRequest{OperationID: "88888888-8888-4888-8888-888888888893",
			Total: 1, MemberHash: updated.MemberHash})
	require.ErrorIs(t, err, store.ErrProductionRevisionConflict)
	_, err = first.ReviewProductionMember(t.Context(), "synthetic-operator", set.ID, 1, 3,
		"88888888-8888-4888-8888-888888888894", api.ProductionMemberReviewRequest{
			OperationID: "88888888-8888-4888-8888-888888888895",
			Binding:     "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Complete: true})
	require.ErrorIs(t, err, store.ErrInvalidProduction)
	_, err = second.ProductionSet(t.Context(), set.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = first.ProductionMapChunk(t.Context(), set.ID, 1,
		"88888888-8888-4888-8888-888888888887", "", 17)
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = second.ProductionMapChunk(t.Context(), set.ID, 1,
		"88888888-8888-4888-8888-888888888887", "", 17)
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = first.ResolveProductionSelection(t.Context(), set.ID, 1, 3,
		api.ProductionResolveRequest{MemberID: "88888888-8888-4888-8888-888888888887", Page: 1})
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = second.ResolveProductionSelection(t.Context(), set.ID, 1, 3,
		api.ProductionResolveRequest{MemberID: "88888888-8888-4888-8888-888888888887", Page: 1})
	require.ErrorIs(t, err, store.ErrNotFound)
	finalize := api.ProductionFinalizeRequest{OperationID: "88888888-8888-4888-8888-888888888897",
		NamespaceID: "88888888-8888-4888-8888-888888888898",
		SnapshotID:  "88888888-8888-4888-8888-888888888899"}
	_, err = first.FinalizeProductionDraft(t.Context(), "synthetic-operator", set.ID, 1, 3, finalize)
	require.ErrorIs(t, err, store.ErrInvalidProduction)
	_, err = second.FinalizeProductionDraft(t.Context(), "synthetic-operator", set.ID, 1, 3, finalize)
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = first.ProductionJobStatus(t.Context(), set.ID, "88888888-8888-4888-8888-888888888887")
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = second.ProductionJobStatus(t.Context(), set.ID, "88888888-8888-4888-8888-888888888887")
	require.ErrorIs(t, err, store.ErrNotFound)
	admission := api.ProductionJobAdmissionRequest{JobID: "88888888-8888-4888-8888-888888888887",
		OperationID: "88888888-8888-4888-8888-888888888885"}
	_, err = first.AdmitProductionJob(t.Context(), set.ID, 1, 1, admission)
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = second.AdmitProductionJob(t.Context(), set.ID, 1, 1, admission)
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = first.CancelProductionJob(t.Context(), "synthetic-operator", set.ID,
		"88888888-8888-4888-8888-888888888887", 1,
		api.ProductionJobCancelRequest{OperationID: "88888888-8888-4888-8888-888888888886"})
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = second.CancelProductionJob(t.Context(), "synthetic-operator", set.ID,
		"88888888-8888-4888-8888-888888888887", 1,
		api.ProductionJobCancelRequest{OperationID: "88888888-8888-4888-8888-888888888886"})
	require.ErrorIs(t, err, store.ErrNotFound)
}
