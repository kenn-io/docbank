package query

import (
	"errors"
	"slices"
	"strings"

	"go.kenn.io/docbank/document"
)

type PersonFilter struct {
	PersonID string   `json:"person_id"`
	Roles    []string `json:"roles"`
}

func normalizePeople(values []PersonFilter) ([]PersonFilter, error) {
	if len(values) == 0 {
		return nil, nil
	}
	if len(values) > 64 {
		return nil, errors.New("filters.people exceeds 64 entries")
	}
	result := make([]PersonFilter, 0, len(values))
	for _, entry := range values {
		if !validUUIDv4(entry.PersonID) || len(entry.Roles) == 0 || len(entry.Roles) > 64 {
			return nil, errors.New("invalid filters.people entry")
		}
		roles := slices.Clone(entry.Roles)
		for _, role := range roles {
			if !document.ValidPersonRole(role) {
				return nil, errors.New("invalid filters.people role")
			}
		}
		slices.Sort(roles)
		entry.Roles = slices.Compact(roles)
		result = append(result, entry)
	}
	slices.SortFunc(result, func(a, b PersonFilter) int {
		if c := strings.Compare(a.PersonID, b.PersonID); c != 0 {
			return c
		}
		return slices.Compare(a.Roles, b.Roles)
	})
	result = slices.CompactFunc(result, func(a, b PersonFilter) bool {
		return a.PersonID == b.PersonID && slices.Equal(a.Roles, b.Roles)
	})
	return result, nil
}
