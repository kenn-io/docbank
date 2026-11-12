package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/docbank/document"

	"go.kenn.io/docbank/internal/query"
)

type compiledRoleMatch struct {
	label     string
	predicate compiledQueryFragment
}

func (c queryCompiler) collectRoleMatches(resolved query.ResolvedQuery) ([]compiledRoleMatch, error) {
	byRole := map[string][]compiledQueryFragment{}
	add := func(label string, fragment compiledQueryFragment) { byRole[label] = append(byRole[label], fragment) }
	var walk func(*query.ResolvedExpression, string, bool) error
	walk = func(e *query.ResolvedExpression, field string, negated bool) error {
		if e == nil || e.Syntax == nil {
			return nil
		}
		if e.Syntax.Kind == query.ExpressionNot {
			return nil
		}
		if e.Syntax.Kind == query.ExpressionField {
			field = e.Syntax.Field
		}
		if e.Saved != nil {
			for _, p := range e.Saved.Query.Filters.People {
				for _, role := range p.Roles {
					add(role, c.compilePersonPredicate(p.PersonID, []string{role}))
				}
			}
			if err := walk(e.Saved.Expression, "", negated); err != nil {
				return err
			}
		}
		if e.Dependency != nil && !negated {
			if field == "mentions" {
				f, err := c.compileExpressionLeaf(e, field)
				if err != nil {
					return err
				}
				add("mentioned", f)
			} else if slices.Contains([]string{"author", "from", "to", "cc", "bcc", "custodian", "participant", "organizer", "attendee", "between"}, field) {
				roles, err := rolesForOperand(field, c.safePeople)
				if err != nil {
					return err
				}
				for _, role := range roles {
					if c.safePeople && role == "blind_copy" {
						continue
					}
					add(role, c.compilePersonPredicate(e.Dependency.ID, []string{role}))
				}
			}
		}
		for _, child := range e.Children {
			if err := walk(child, field, negated); err != nil {
				return err
			}
		}
		return nil
	}
	if !c.photoDisplayMetadata {
		if err := walk(resolved.Expression, "", false); err != nil {
			return nil, err
		}
		for _, p := range resolved.Query.Filters.People {
			for _, role := range p.Roles {
				add(role, c.compilePersonPredicate(p.PersonID, []string{role}))
			}
		}
	}
	keys := make([]string, 0, len(byRole))
	for key := range byRole {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	result := make([]compiledRoleMatch, 0, len(keys))
	for _, key := range keys {
		result = append(result, compiledRoleMatch{key, joinCompiledFragments(byRole[key], ` OR `)})
	}
	return result, nil
}

func annotateMatchedRoles(ctx context.Context, q metadataQuerier, compiled CompiledQuery, generation string, coverage CoverageSelection, rows []SnapshotRow) error {
	if len(rows) == 0 {
		return nil
	}
	ids := make([]int64, len(rows))
	index := make(map[int64]int, len(rows))
	for i, row := range rows {
		ids[i] = row.NodeID
		index[row.NodeID] = i
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	for _, match := range compiled.roleMatches {
		partial := compiled
		partial.predicate = match.predicate
		partial.Query.Filters.CollapseDuplicates = false
		population, err := matchedPopulation(partial, generation, nil)
		if err != nil {
			return err
		}
		statement, args, err := bindQueryPopulation(population, coverage, generation)
		if err != nil {
			return err
		}
		args = append(args, string(encoded))
		found, err := q.QueryContext(ctx, `SELECT node_id FROM (`+statement+`) WHERE node_id IN (SELECT value FROM json_each(?))`, args...)
		if err != nil {
			return fmt.Errorf("annotating roles: %w", err)
		}
		for found.Next() {
			var id int64
			if err := found.Scan(&id); err != nil {
				_ = found.Close()
				return err
			}
			if i, ok := index[id]; ok {
				rows[i].MatchedRoles = append(rows[i].MatchedRoles, match.label)
			}
		}
		err = found.Err()
		_ = found.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// materializePersonRoleFacet counts each current version once per visible person.
func materializePersonRoleFacet(ctx context.Context, q metadataQuerier, compiled CompiledQuery, generation string, coverage CoverageSelection, memberLimit int64) (SnapshotFacet, error) {
	value := queryWithoutSnapshotFacet(compiled.Query, "person_role")
	scoped, err := (queryCompiler{}).compile(ctx, value, queryResolver{q: q})
	if err != nil {
		return SnapshotFacet{}, err
	}
	population, err := matchedPopulation(scoped, generation, nil)
	if err != nil {
		return SnapshotFacet{}, err
	}
	statement, args, err := bindQueryPopulation(population, coverage, generation)
	if err != nil {
		return SnapshotFacet{}, err
	}
	hide := "1"
	if visible, _ := ctx.Value(visiblePersonContextKey{}).(bool); visible && !queryPeopleHideLocked(ctx) {
		hide = "0"
	}
	args = append(args, document.PersonResolverFingerprint())
	sql := `WITH matched_members AS (` + statement + `), edges AS (
 SELECT n.id node_id,COALESCE((SELECT surviving_person_id FROM person_aliases WHERE retired_person_id=a.person_id),a.person_id) person_id,a.role
 FROM matched_members m JOIN nodes n ON n.id=m.node_id JOIN person_document_assertions a ON a.content_version_id=m.content_version_id
 WHERE ` + currentPersonAssertionFence(true, hide) + ` AND a.role IN (` + eventRolesSQL() + `)
 UNION SELECT n.id,COALESCE((SELECT surviving_person_id FROM person_aliases WHERE retired_person_id=p.person_id),p.person_id),p.role
 FROM matched_members m JOIN nodes n ON n.id=m.node_id JOIN document_people p ON p.content_version_id=m.content_version_id
 JOIN document_people_heads h ON h.content_version_id=p.content_version_id AND h.generation_id=p.generation_id
 JOIN document_event_heads eh ON eh.content_version_id=h.content_version_id CROSS JOIN document_people_state ps
 WHERE ` + currentPersonPublishedFence(true, hide) + ` AND p.role IN (` + eventRolesSQL() + `))
 SELECT m.node_id,COALESCE(p.person_id,''),COALESCE(p.display_name,''),COALESCE(e.role,'') FROM matched_members m LEFT JOIN edges e ON e.node_id=m.node_id LEFT JOIN persons p ON p.person_id=e.person_id AND p.state<>'retired' ORDER BY m.node_id,p.person_id,e.role`
	rows, err := q.QueryContext(ctx, sql, args...)
	if err != nil {
		return SnapshotFacet{}, fmt.Errorf("materializing people facet: %w", err)
	}
	defer func() { _ = rows.Close() }()
	counts := map[string]int64{}
	labels := map[string]string{}
	roles := map[string]map[string]bool{}
	var lastNode int64
	var lastPerson string
	var total, missing int64
	var nodeHasPerson bool
	for rows.Next() {
		var node int64
		var id, name, role string
		if err := rows.Scan(&node, &id, &name, &role); err != nil {
			return SnapshotFacet{}, err
		}
		if node != lastNode {
			if lastNode != 0 && !nodeHasPerson {
				missing++
			}
			total++
			if total > memberLimit {
				return unavailableSnapshotFacet("person_role", "member_budget_exceeded"), nil
			}
			lastNode = node
			lastPerson = ""
			nodeHasPerson = false
		}
		if id == "" {
			continue
		}
		nodeHasPerson = true
		labels[id] = name
		if id != lastPerson {
			counts[id]++
			lastPerson = id
		}
		if roles[id] == nil {
			roles[id] = map[string]bool{}
		}
		roles[id][role] = true
	}
	if err := rows.Err(); err != nil {
		return SnapshotFacet{}, err
	}
	if lastNode != 0 && !nodeHasPerson {
		missing++
	}
	if err := rows.Close(); err != nil {
		return SnapshotFacet{}, err
	}
	selected := map[string]bool{}
	for _, entry := range compiled.Query.Filters.People {
		var id, name string
		err := q.QueryRowContext(ctx, `SELECT person_id,display_name FROM persons WHERE person_id=COALESCE((SELECT surviving_person_id FROM person_aliases WHERE retired_person_id=?),?) AND `+personVisibleSQL("persons.person_id", 3), entry.PersonID, entry.PersonID, 1, boolInt(hide == "1")).Scan(&id, &name)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return SnapshotFacet{}, err
		}
		if roles[id] == nil {
			roles[id] = map[string]bool{}
			for _, role := range entry.Roles {
				if role != "blind_copy" {
					roles[id][role] = true
				}
			}
		}
		if len(roles[id]) == 0 {
			continue
		}
		selected[id] = true
		labels[id] = name
	}
	values := make([]SnapshotFacetValue, 0, len(counts))
	for id, count := range counts {
		values = append(values, SnapshotFacetValue{Key: id, Label: labels[id], Count: count, Selected: selected[id]})
	}
	slices.SortFunc(values, func(a, b SnapshotFacetValue) int {
		if a.Count > b.Count {
			return -1
		}
		if a.Count < b.Count {
			return 1
		}
		return strings.Compare(a.Key, b.Key)
	})
	var other int64
	included := map[string]bool{}
	out := make([]SnapshotFacetValue, 0, min(20, len(values))+len(selected))
	for i, v := range values {
		if i < 20 {
			out = append(out, v)
			included[v.Key] = true
		} else if !selected[v.Key] {
			other += v.Count
		}
	}
	var extra []string
	for id := range selected {
		if !included[id] {
			extra = append(extra, id)
		}
	}
	slices.Sort(extra)
	for _, id := range extra {
		out = append(out, SnapshotFacetValue{Key: id, Label: labels[id], Count: counts[id], Selected: true})
	}
	for i := range out {
		for role := range roles[out[i].Key] {
			out[i].Roles = append(out[i].Roles, role)
		}
		slices.Sort(out[i].Roles)
	}
	return SnapshotFacet{Dimension: "person_role", Available: true, Total: &total, Values: out, Missing: &missing, Other: &other}, nil
}

// Event roles are the only roles accepted by the facet wire contract.
func eventRolesSQL() string {
	roles := document.AllEventRoles()
	values := make([]string, 0, len(roles))
	for _, role := range roles {
		values = append(values, "'"+strings.ReplaceAll(string(role), "'", "''")+"'")
	}
	return strings.Join(values, ",")
}
