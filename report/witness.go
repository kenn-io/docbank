package report

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// WitnessDigest binds one collection membership claim to its node and source
// provenance. Offline verification can check the claim's internal receipt;
// source authenticity still requires the original vault.
func WitnessDigest(nodeID int64, witness CollectionWitness) string {
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "node:%d", nodeID)
	for _, value := range []string{
		witness.CollectionID, witness.MembershipID, witness.OriginalPath,
		witness.OriginalMTime, witness.Supersedes,
	} {
		_, _ = fmt.Fprintf(hash, "%d:%s", len(value), value)
	}
	return hex.EncodeToString(hash.Sum(nil))
}
