package production

import (
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	documentproduction "go.kenn.io/docbank/document/production"
)

func TestPrivilegePublicProjectionAllowlist(t *testing.T) {
	_, withheld, players, rows := internalPrivilegeFixture(t)
	rows[0].PrivateRationale = "PRIVATE-RATIONALE-CANARY"
	rows[0].EvidenceSHA256 = strings.Repeat("e", 64)
	rows[0].PublicDescription = `=HYPERLINK("https://example.test","Synthetic")`
	rows[0].Fields = append(rows[0].Fields, documentproduction.PrivilegeField{Name: "private_note", Value: "PRIVATE-FIELD-CANARY"})
	players.Players[0].Aliases = append(players.Players[0].Aliases, "PRIVATE-ALIAS-CANARY")
	input := documentproduction.PrivilegeLogFreezeInput{
		LogID: withheld.ID, Revision: withheld.Revision, WithheldSelectionSHA256: withheld.SHA256,
		PolicySHA256: withheld.PolicySHA256, PlayersSHA256: players.SHA256,
		ValidatedAt: "2026-09-22T03:00:00Z", Rows: rows, ApprovalEvaluationSHA256: approvalTestSHA("a"), FrozenAt: "2026-09-22T03:00:01Z"}
	receipt, err := documentproduction.FreezePrivilegeLog(input)
	require.NoError(t, err)

	jsonExport, err := ExportPrivilegeLogJSON(receipt, withheld, rows)
	require.NoError(t, err)
	csvExport, err := ExportPrivilegeLogCSV(receipt, withheld, rows)
	require.NoError(t, err)
	for _, exported := range []PrivilegeLogExport{jsonExport, csvExport} {
		require.Equal(t, receipt.SHA256, exported.ReceiptSHA256)
		require.Equal(t, receipt.RowsSHA256, exported.RowsSHA256)
		require.NotEmpty(t, exported.ContentSHA256)
		require.NotContains(t, string(exported.Content), "PRIVATE-")
		require.NotContains(t, string(exported.Content), rows[0].EvidenceSHA256)
	}

	var publicRows []map[string]any
	require.NoError(t, json.Unmarshal(jsonExport.Content, &publicRows))
	require.Equal(t, []string{"basis", "family_order", "id", "public_description", "source_version_id", "withheld_member_id"}, sortedMapKeys(publicRows[0]))

	parsedCSV, err := csv.NewReader(strings.NewReader(string(csvExport.Content))).ReadAll()
	require.NoError(t, err)
	require.Equal(t, []string{"id", "withheld_member_id", "family_order", "source_version_id", "basis", "public_description"}, parsedCSV[0])
	require.Len(t, parsedCSV, 2)
	require.Equal(t, `'=`+strings.TrimPrefix(rows[0].PublicDescription, "="), parsedCSV[1][5])

	reordered := append([]documentproduction.PrivilegeRow(nil), rows...)
	for left, right := 0, len(reordered)-1; left < right; left, right = left+1, right-1 {
		reordered[left], reordered[right] = reordered[right], reordered[left]
	}
	reorderedJSON, err := ExportPrivilegeLogJSON(receipt, withheld, reordered)
	require.NoError(t, err)
	require.Equal(t, jsonExport.Content, reorderedJSON.Content)
	require.Equal(t, jsonExport.ContentSHA256, reorderedJSON.ContentSHA256)
}

func TestPrivilegeCSVFormulaPrefixesAndContentDigest(t *testing.T) {
	for _, prefix := range []string{"=", "+", "-", "@"} {
		t.Run(prefix, func(t *testing.T) {
			_, withheld, players, rows := internalPrivilegeFixture(t)
			rows[0].Basis = prefix + "synthetic_basis"
			rows[0].PublicDescription = prefix + "synthetic description"
			receipt, err := documentproduction.FreezePrivilegeLog(documentproduction.PrivilegeLogFreezeInput{
				LogID: withheld.ID, Revision: withheld.Revision, WithheldSelectionSHA256: withheld.SHA256,
				PolicySHA256: withheld.PolicySHA256, PlayersSHA256: players.SHA256,
				ValidatedAt: "2026-09-22T03:00:00Z", Rows: rows, FrozenAt: "2026-09-22T03:00:01Z",
			})
			require.NoError(t, err)

			exported, err := ExportPrivilegeLogCSV(receipt, withheld, rows)
			require.NoError(t, err)
			parsed, err := csv.NewReader(strings.NewReader(string(exported.Content))).ReadAll()
			require.NoError(t, err)
			require.Equal(t, "'"+rows[0].Basis, parsed[1][4])
			require.Equal(t, "'"+rows[0].PublicDescription, parsed[1][5])
			digest := sha256.Sum256(exported.Content)
			require.Equal(t, hex.EncodeToString(digest[:]), exported.ContentSHA256,
				"content digest must cover the formula-escaped CSV bytes")
		})
	}
}

func TestPrivilegeExportAndAttachmentRequireFrozenRows(t *testing.T) {
	_, withheld, players, rows := internalPrivilegeFixture(t)
	receipt, err := documentproduction.FreezePrivilegeLog(documentproduction.PrivilegeLogFreezeInput{
		LogID: withheld.ID, Revision: withheld.Revision, WithheldSelectionSHA256: withheld.SHA256,
		PolicySHA256: withheld.PolicySHA256, PlayersSHA256: players.SHA256,
		ValidatedAt: "2026-09-22T03:00:00Z", Rows: rows, FrozenAt: "2026-09-22T03:00:01Z",
	})
	require.NoError(t, err)
	changed := clonePrivilegeRows(rows)
	changed[0].PublicDescription = "Changed public description."
	_, err = ExportPrivilegeLogJSON(receipt, withheld, changed)
	require.Error(t, err)

	originalReceiptDigest := receipt.SHA256
	attachment, err := PreparePrivilegeLogAttachment(
		"11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222",
		receipt, withheld, rows, approvalTestSHA("4"), []documentproduction.PrivilegeOutputReference{{
			WithheldMemberID: rows[0].WithheldMemberID, AssignedNumber: "SYN000001", ArtifactSHA256: approvalTestSHA("5"),
		}}, mustApprovalTime(t, "2026-09-22T04:00:00Z"),
	)
	require.NoError(t, err)
	require.Equal(t, originalReceiptDigest, receipt.SHA256, "later number references do not mutate the frozen receipt")
	require.Equal(t, receipt.SHA256, attachment.Receipt.PrivilegeLogReceiptSHA256)
	require.NotEqual(t, receipt.SHA256, attachment.Receipt.SHA256)

	_, err = PreparePrivilegeLogAttachment(
		"33333333-3333-4333-8333-333333333333", "44444444-4444-4444-8444-444444444444",
		receipt, withheld, rows, approvalTestSHA("4"), []documentproduction.PrivilegeOutputReference{{
			WithheldMemberID: "55555555-5555-4555-8555-555555555555", AssignedNumber: "SYN000002", ArtifactSHA256: approvalTestSHA("6"),
		}}, time.Date(2026, 9, 22, 4, 0, 0, 0, time.UTC),
	)
	require.Error(t, err, "number references must name a member in the frozen rows")
}

func sortedMapKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
