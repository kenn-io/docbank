package api

import (
	"fmt"

	"go.kenn.io/docbank/internal/store"
)

// ParseVersionPruneRequest converts the wire selector into the store's
// authoritative selector type. It also preserves the distinction between an
// omitted age and a supplied zero age, which time.Duration alone cannot carry.
func ParseVersionPruneRequest(request VersionPruneRequest) (store.VersionPruneSelector, error) {
	age, err := ParseAge(request.OlderThan)
	if err != nil {
		return store.VersionPruneSelector{}, fmt.Errorf(
			"invalid version-prune age %q: %w", request.OlderThan, err,
		)
	}
	if request.OlderThan != "" && age == 0 {
		return store.VersionPruneSelector{}, fmt.Errorf(
			"version-prune age must be greater than zero: %w", store.ErrInvalidVersionPrune,
		)
	}
	selector := store.VersionPruneSelector{
		VersionIDs: request.VersionIDs,
		KeepNewest: request.KeepNewest,
		OlderThan:  age,
		AllPrior:   request.AllPrior,
	}
	if err := store.ValidateVersionPruneSelector(selector); err != nil {
		return store.VersionPruneSelector{}, err
	}
	return selector, nil
}
