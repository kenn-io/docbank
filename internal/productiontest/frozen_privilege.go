package productiontest

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	productionservice "go.kenn.io/docbank/internal/production"
	"go.kenn.io/docbank/internal/store"
)

type PrivilegeDraft struct {
	LogID      string
	Revision   int64
	Generation int64
	Rows       []documentproduction.PrivilegeRow
}

func SeedPrivilegeLogDraft(t *testing.T, s *store.Store) PrivilegeDraft {
	t.Helper()
	sha := func(char string) string { return strings.Repeat(char, 64) }
	policy := documentproduction.PolicyVersion{
		Contract: documentproduction.PolicyContractV1,
		ID:       "11111111-1111-4111-8111-111111111111", Version: 1,
		Name: "Synthetic privilege policy", CreatedAt: "2026-09-22T13:00:00Z",
		Rules: []documentproduction.PolicyRule{{
			ID: "withhold-selected", Kind: documentproduction.PolicyRuleDisposition,
			Predicate:   documentproduction.PolicyPredicate{Field: "member.id", Operator: documentproduction.PolicyOperatorPresent},
			Disposition: documentproduction.PolicyDispositionWithhold,
		}},
		PrivilegeLog: documentproduction.PrivilegeLogRequirement{Required: true, RequireFrozenReceipt: true,
			RequiredFields: []string{"date"}, AllowedBases: []string{"synthetic_basis"}},
		ConflictMode: documentproduction.PolicyConflictReject,
	}
	preparedPolicy, err := productionservice.PreparePolicyVersion("22222222-2222-4222-8222-222222222222", policy)
	require.NoError(t, err)
	policy, err = s.PutProductionPolicy(t.Context(), preparedPolicy)
	require.NoError(t, err)
	players, err := productionservice.PreparePlayersSnapshot("33333333-3333-4333-8333-333333333333",
		documentproduction.PlayersSnapshot{Contract: documentproduction.PlayersSnapshotContractV1,
			ID: "44444444-4444-4444-8444-444444444444", Revision: 1,
			Players: []documentproduction.Player{{ID: "55555555-5555-4555-8555-555555555555",
				DisplayName: "Synthetic Person", Aliases: []string{"synthetic@example.test"}, EvidenceSHA256: sha("1")}},
		})
	require.NoError(t, err)
	_, err = s.PutProductionPlayersSnapshot(t.Context(), players)
	require.NoError(t, err)
	withheld, err := productionservice.PrepareWithheldSelection("66666666-6666-4666-8666-666666666666",
		documentproduction.WithheldSelection{
			Contract: documentproduction.WithheldSelectionContractV1,
			ID:       "77777777-7777-4777-8777-777777777777",
			SetID:    "88888888-8888-4888-8888-888888888888", Revision: 1,
			PolicySHA256: policy.SHA256,
			Members: []documentproduction.WithheldMember{{
				ID: "99999999-9999-4999-8999-999999999999", Ordinal: 1,
				SourceVersionID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", SourceSHA256: sha("2"), SourceSize: 10,
				FamilyOrder: 1, Family: redaction.FamilyContext{Kind: "standalone",
					RootVersionID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"},
			}, {
				ID: "abababab-abab-4bab-8bab-abababababab", Ordinal: 2,
				SourceVersionID: "bcbcbcbc-bcbc-4cbc-8cbc-bcbcbcbcbcbc", SourceSHA256: sha("4"), SourceSize: 12,
				FamilyOrder: 1, Family: redaction.FamilyContext{Kind: "standalone",
					RootVersionID: "bcbcbcbc-bcbc-4cbc-8cbc-bcbcbcbcbcbc"},
			}},
		}, nil)
	require.NoError(t, err)
	_, err = s.PutProductionWithheldSelection(t.Context(), withheld)
	require.NoError(t, err)
	draft, err := productionservice.PreparePrivilegeLogDraft(productionservice.PrivilegeLogDraftRequest{
		OperationID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", LogID: "13131313-1313-4313-8313-131313131313",
		Revision: 1,
	}, withheld.Selection, policy)
	require.NoError(t, err)
	rows := []documentproduction.PrivilegeRow{{
		ID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", WithheldMemberID: withheld.Selection.Members[0].ID,
		FamilyOrder: 1, SourceVersionID: withheld.Selection.Members[0].SourceVersionID,
		Basis: "synthetic_basis", PublicDescription: "Synthetic public description.",
		PrivateRationale: "Synthetic private rationale.", EvidenceSHA256: sha("3"),
		PersonIDs: []string{players.Snapshot.Players[0].ID},
		Fields:    []documentproduction.PrivilegeField{{Name: "date", Value: "2026-09-22"}},
	}, {
		ID: "cdcdcdcd-cdcd-4dcd-8dcd-cdcdcdcdcdcd", WithheldMemberID: withheld.Selection.Members[1].ID,
		FamilyOrder: 1, SourceVersionID: withheld.Selection.Members[1].SourceVersionID,
		Basis: "synthetic_basis", PublicDescription: "Second synthetic public description.",
		PrivateRationale: "Second synthetic private rationale.", EvidenceSHA256: sha("5"),
		PersonIDs: []string{players.Snapshot.Players[0].ID},
		Fields:    []documentproduction.PrivilegeField{{Name: "date", Value: "2026-09-23"}},
	}}
	generation, err := s.CreatePrivilegeLogDraft(t.Context(), store.PrivilegeLogDraftAuthority{
		Draft: draft, PlayersSHA256: players.SnapshotSHA256, Rows: rows,
	})
	require.NoError(t, err)
	return PrivilegeDraft{LogID: draft.LogID, Revision: draft.Revision, Generation: generation, Rows: rows}
}

func SeedFrozenPrivilegeLog(t *testing.T, s *store.Store) {
	t.Helper()
	draft := SeedPrivilegeLogDraft(t, s)
	validation, err := productionservice.ValidateStoredPrivilegeLog(t.Context(), s,
		productionservice.PrivilegeLogValidationRequest{
			OperationID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd", LogID: draft.LogID,
			Revision: draft.Revision, ExpectedGeneration: draft.Generation,
			ValidatedAt: time.Date(2026, 9, 22, 14, 0, 0, 0, time.UTC),
		})
	require.NoError(t, err)
	_, err = productionservice.FreezeStoredPrivilegeLog(t.Context(), s,
		productionservice.PrivilegeLogFreezeRequest{
			OperationID: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", LogID: draft.LogID,
			Revision: draft.Revision, ExpectedGeneration: draft.Generation,
			ExpectedInputsSHA256: validation.Validation.InputsSHA256,
			FrozenAt:             time.Date(2026, 9, 22, 14, 1, 0, 0, time.UTC),
		})
	require.NoError(t, err)
}
