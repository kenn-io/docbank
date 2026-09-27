package store

import (
	"context"
	"errors"
	"strconv"
	"strings"

	documentproduction "go.kenn.io/docbank/document/production"
)

const MaxProductionPolicyPage = 100

type ProductionPolicyPage struct {
	Items      []documentproduction.PolicyVersion
	NextCursor string
}

// ListProductionPolicies pages immutable policy versions in ID/version order.
// A cursor names the final version from the previous page.
func (s *Store) ListProductionPolicies(ctx context.Context, cursor string, limit int) (ProductionPolicyPage, error) {
	if limit < 1 || limit > MaxProductionPolicyPage || len(cursor) > 60 {
		return ProductionPolicyPage{}, invalidProductionStorage("invalid production policy page")
	}
	afterID := ""
	var afterVersion int64
	if cursor != "" {
		id, versionText, ok := strings.Cut(cursor, ":")
		version, err := strconv.ParseInt(versionText, 10, 64)
		if !ok || validateUUIDv4(id) != nil || err != nil || version < 1 ||
			strconv.FormatInt(version, 10) != versionText {
			return ProductionPolicyPage{}, invalidProductionStorage("invalid production policy cursor")
		}
		afterID, afterVersion = id, version
	}
	rows, err := s.db.QueryContext(ctx, `SELECT policy_id,version,canonical_json,sha256
		FROM production_policy_versions
		WHERE policy_id > ? OR (policy_id = ? AND version > ?)
		ORDER BY policy_id,version LIMIT ?`, afterID, afterID, afterVersion, limit+1)
	if err != nil {
		return ProductionPolicyPage{}, err
	}
	defer func() { _ = rows.Close() }()
	page := ProductionPolicyPage{Items: make([]documentproduction.PolicyVersion, 0, limit)}
	for rows.Next() {
		var id, digest string
		var version int64
		var raw []byte
		if err := rows.Scan(&id, &version, &raw, &digest); err != nil {
			return ProductionPolicyPage{}, err
		}
		policy, err := decodeProductionPolicy(raw, digest)
		if err != nil {
			return ProductionPolicyPage{}, err
		}
		if policy.ID != id || policy.Version != version {
			return ProductionPolicyPage{}, errors.New("production policy list contains invalid authority")
		}
		if len(page.Items) == limit {
			last := page.Items[len(page.Items)-1]
			page.NextCursor = last.ID + ":" + strconv.FormatInt(last.Version, 10)
			break
		}
		page.Items = append(page.Items, policy)
	}
	if err := rows.Err(); err != nil {
		return ProductionPolicyPage{}, err
	}
	return page, nil
}
