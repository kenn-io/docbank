package production

import (
	"slices"
	"strings"

	"go.kenn.io/docbank/document"
	documentproduction "go.kenn.io/docbank/document/production"
)

// PreparedPlayersSnapshot is the exact immutable player authority handed to
// coordinator-owned persistence.
type PreparedPlayersSnapshot struct {
	OperationID    string
	RequestSHA256  string
	SnapshotSHA256 string
	Canonical      []byte
	Snapshot       documentproduction.PlayersSnapshot
}

// PreparePlayersSnapshot canonicalizes one caller-authored revision. The
// service computes its digest; callers cannot bless their own self-digest.
func PreparePlayersSnapshot(operationID string, value documentproduction.PlayersSnapshot) (PreparedPlayersSnapshot, error) {
	if !canonicalUUIDv4(operationID) || value.SHA256 != "" {
		return PreparedPlayersSnapshot{}, invalidProductionContract("invalid players snapshot request")
	}
	encoded, digest, err := documentproduction.CanonicalPlayersSnapshot(value)
	if err != nil {
		return PreparedPlayersSnapshot{}, err
	}
	value.SHA256 = digest
	return PreparedPlayersSnapshot{
		OperationID: operationID, RequestSHA256: digest, SnapshotSHA256: digest,
		Canonical: encoded, Snapshot: value,
	}, nil
}

// ResolvePlayerAlias resolves within one exact stored snapshot. No match and
// multiple matches are explicit validation failures; the service never guesses.
func ResolvePlayerAlias(snapshot documentproduction.PlayersSnapshot, alias string) (documentproduction.Player, error) {
	if err := documentproduction.ValidatePlayersSnapshot(snapshot); err != nil {
		return documentproduction.Player{}, err
	}
	if !document.ValidPersonIdentityText(alias) || strings.TrimSpace(alias) == "" {
		return documentproduction.Player{}, invalidProductionContract("invalid player alias")
	}
	normalized := document.FoldPersonName(alias)
	matches := make(map[string]documentproduction.Player)
	for _, player := range snapshot.Players {
		values := append(slices.Clone(player.Aliases), player.DisplayName)
		for _, candidate := range values {
			if document.FoldPersonName(candidate) == normalized {
				matches[player.ID] = player
				break
			}
		}
	}
	ids := make([]string, 0, len(matches))
	for id := range matches {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	if len(ids) != 1 {
		return documentproduction.Player{}, &documentproduction.Problem{
			Code: documentproduction.ProblemPolicyUnsatisfied, Detail: "player alias does not resolve uniquely", IDs: ids,
		}
	}
	return matches[ids[0]], nil
}
