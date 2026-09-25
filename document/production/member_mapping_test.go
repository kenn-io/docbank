package production

import (
	"crypto/sha256"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/canonical"
)

func TestMemberOrdinalMappings(t *testing.T) {
	for _, test := range []struct {
		name          string
		secondMember  string
		secondOrdinal int64
		valid         bool
	}{
		{"distinct members", memberTwoID, 2, true},
		{"repeated member", memberOneID, 1, true},
		{"member changes ordinal", memberOneID, 2, false},
		{"ordinal changes member", memberTwoID, 1, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Run("artifact manifest", func(t *testing.T) {
				manifest := ArtifactManifest{Contract: ArtifactManifestContractV1, Artifacts: []Artifact{
					{ID: "13131313-1313-4313-8313-131313131313", MemberID: memberOneID, MemberOrdinal: 1, Page: 1, Role: ArtifactRoleRedactedPage, Path: "VOL001/SYN000001.png", SHA256: sha("1"), Size: 10, MediaType: "image/png", Volume: "VOL001"},
					{ID: "14141414-1414-4414-8414-141414141414", MemberID: test.secondMember, MemberOrdinal: test.secondOrdinal, Page: 2, Role: ArtifactRoleRedactedPage, Path: "VOL001/SYN000002.png", SHA256: sha("2"), Size: 10, MediaType: "image/png", Volume: "VOL001"},
				}}
				// The payload is already ordered; hash it without contract validation so
				// the stored check exercises member mapping rather than digest mismatch.
				raw, err := canonical.Marshal(manifest)
				require.NoError(t, err)
				manifest.SHA256 = fmt.Sprintf("%x", sha256.Sum256(raw))
				t.Run("canonical", func(t *testing.T) {
					_, _, err := CanonicalArtifactManifest(manifest)
					if test.valid {
						require.NoError(t, err)
					} else {
						requireProblemCode(t, err, ProblemInvalidContract)
					}
				})
				t.Run("stored", func(t *testing.T) {
					err := ValidateArtifactManifest(manifest)
					if test.valid {
						require.NoError(t, err)
					} else {
						requireProblemCode(t, err, ProblemInvalidContract)
					}
				})
			})
			t.Run("artifact provenance", func(t *testing.T) {
				receipt := ArtifactProvenanceReceipt{
					Contract: ArtifactProvenanceContractV1, ID: "20202020-2020-4020-8020-202020202020",
					ProductionReceiptSHA256: sha("2"), ArtifactManifestSHA256: sha("3"), CreatedAt: "2026-09-22T04:00:00Z",
					Entries: []ArtifactProvenance{
						{ArtifactID: "21212121-2121-4121-8121-212121212121", ArtifactSHA256: sha("4"), SourceVersionID: versionOneID, MemberID: memberOneID, MemberOrdinal: 1, Page: 1, Volume: "VOL001"},
						{ArtifactID: "23232323-2323-4323-8323-232323232323", ArtifactSHA256: sha("5"), SourceVersionID: versionOneID, MemberID: test.secondMember, MemberOrdinal: test.secondOrdinal, Page: 2, Volume: "VOL001"},
					},
				}
				raw, err := canonical.Marshal(receipt)
				require.NoError(t, err)
				receipt.SHA256 = fmt.Sprintf("%x", sha256.Sum256(raw))
				t.Run("canonical", func(t *testing.T) {
					_, _, err := CanonicalArtifactProvenanceReceipt(receipt)
					if test.valid {
						require.NoError(t, err)
					} else {
						requireProblemCode(t, err, ProblemInvalidContract)
					}
				})
				t.Run("stored", func(t *testing.T) {
					err := ValidateArtifactProvenanceReceipt(receipt)
					if test.valid {
						require.NoError(t, err)
					} else {
						requireProblemCode(t, err, ProblemInvalidContract)
					}
				})
			})
			t.Run("number reservation", func(t *testing.T) {
				reservation := NumberReservation{
					Contract: NumberReservationContractV1, Authority: "synthetic-numbering-ledger/v1",
					ID: "18181818-1818-4818-8818-181818181818", OperationID: "19191919-1919-4919-8919-191919191919",
					RevisionSHA256: sha("1"), State: "reserved",
					Numbers: []AssignedNumber{
						{MemberID: memberOneID, MemberOrdinal: 1, Page: 1, Text: "SYN000001"},
						{MemberID: test.secondMember, MemberOrdinal: test.secondOrdinal, Page: 2, Text: "SYN000002"},
					},
				}
				raw, err := canonical.Marshal(reservation)
				require.NoError(t, err)
				reservation.SHA256 = fmt.Sprintf("%x", sha256.Sum256(raw))
				t.Run("canonical", func(t *testing.T) {
					_, _, err := CanonicalNumberReservation(reservation)
					if test.valid {
						require.NoError(t, err)
					} else {
						requireProblemCode(t, err, ProblemInvalidContract)
					}
				})
				t.Run("stored", func(t *testing.T) {
					err := ValidateNumberReservation(reservation)
					if test.valid {
						require.NoError(t, err)
					} else {
						requireProblemCode(t, err, ProblemInvalidContract)
					}
				})
			})
		})
	}
}
