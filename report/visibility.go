package report

import "fmt"

// MaxVisibilityIdentities bounds the exact source evidence retained in one
// durable history receipt. The encoded receipt has a separate 8 MiB cap.
const MaxVisibilityIdentities = 50000

// VisibilityIdentities returns each exact document version that contributed
// to an observation, including family relations outside the selected scope.
// The order is stable so receipts can be compared and replayed directly.
func VisibilityIdentities(frame Frame) ([]Identity, error) {
	identities := make([]Identity, 0, min(len(frame.Members), MaxVisibilityIdentities))
	seen := make(map[Identity]bool, cap(identities))
	add := func(identity Identity) error {
		if seen[identity] {
			return nil
		}
		if len(identities) == MaxVisibilityIdentities {
			return fmt.Errorf("%w: too many report visibility dependencies", ErrReportLimit)
		}
		seen[identity] = true
		identities = append(identities, identity)
		return nil
	}
	for _, member := range frame.Members {
		if err := add(member.Identity); err != nil {
			return nil, err
		}
	}
	for _, relation := range frame.Relations {
		if err := add(relation.Parent); err != nil {
			return nil, err
		}
		if err := add(relation.Child); err != nil {
			return nil, err
		}
	}
	return identities, nil
}
