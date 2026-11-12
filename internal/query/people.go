package query

import (
	"errors"
	"slices"

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
			if !slices.Contains(document.AllEventRoles(), document.EventRole(role)) {
				return nil, errors.New("invalid filters.people role")
			}
		}
		slices.Sort(roles)
		entry.Roles = slices.Compact(roles)
		result = append(result, entry)
	}
	return result, nil
}
