package store

import (
	"testing"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
)

func TestStoredWithheldSelectionRequiresExactSealedMember(t *testing.T) {
	for _, test := range []struct {
		name        string
		changedHash bool
		unsealed    bool
	}{
		{name: "exact retained member"},
		{name: "changed source hash", changedHash: true},
		{name: "unsealed revision", unsealed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, member, setID, revision := productionEvidenceRevisionWithPolicy(t, "metadata.title", "standalone", false, true)
			stored := loadProductionInputsForTest(t, s, setID, revision)
			if test.unsealed {
				_, err := s.db.Exec(`UPDATE production_revisions SET membership_sealed=0
					WHERE set_id=? AND revision=?`, setID, revision)
				require.NoError(t, err)
			}
			selected := documentproduction.WithheldMember{
				ID: member.ID, Ordinal: member.Ordinal, SourceVersionID: member.SourceVersionID,
				SourceSHA256: member.SourceSHA256, SourceSize: member.SourceSize,
				FamilyOrder: 1, Family: member.Family,
			}
			if test.changedHash {
				selected.SourceSHA256 = fakeHash("aa")
			}
			selection := documentproduction.WithheldSelection{
				Contract: documentproduction.WithheldSelectionContractV1,
				ID:       "78787878-7878-4787-8787-787878787801", SetID: setID, Revision: revision,
				PolicySHA256: stored.Policy.SHA256,
				Members:      []documentproduction.WithheldMember{selected},
			}
			const operationID = "78787878-7878-4787-8787-787878787802"
			created, err := s.CreateStoredProductionWithheldSelection(t.Context(), operationID, selection)
			if test.changedHash || test.unsealed {
				requireProductionProblem(t, err, documentproduction.ProblemInvalidContract)
				require.Zero(t, created)
				var count int
				require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM production_withheld_selections
					WHERE set_id=? AND revision=?`, setID, revision).Scan(&count))
				require.Zero(t, count)
				return
			}
			require.NoError(t, err)
			require.NotEmpty(t, created.SHA256)
			replay, err := s.CreateStoredProductionWithheldSelection(t.Context(), operationID, selection)
			require.NoError(t, err)
			require.Equal(t, created, replay)
			changed := selection
			changed.ID = "78787878-7878-4787-8787-787878787803"
			_, err = s.CreateStoredProductionWithheldSelection(t.Context(), operationID, changed)
			requireProductionProblem(t, err, documentproduction.ProblemChangedPayload)
		})
	}
}
