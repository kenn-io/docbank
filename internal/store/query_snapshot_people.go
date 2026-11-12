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
	if !c.safePeople {
		c.hideLocked = "1"
	}
	c.safePeople = true
	var walk func(*query.ResolvedExpression, string) error
	walk = func(e *query.ResolvedExpression, field string) error {
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
					if role == "blind_copy" {
						continue
					}
					add(role, c.compilePersonPredicate(p.PersonID, []string{role}))
				}
			}
			if err := walk(e.Saved.Expression, ""); err != nil {
				return err
			}
		}
		if e.Dependency != nil {
			if field == "mentions" {
				f, err := c.compileExpressionLeaf(e, field)
				if err != nil {
					return err
				}
				add("mentioned", f)
			} else if slices.Contains([]string{"author", "from", "to", "cc", "custodian", "participant", "organizer", "attendee", "between"}, field) {
				roles := []string{"author", "last_saved_by", "sender", "recipient", "copied", "organizer", "attendee", "participant", "speaker", "depicted"}
				var err error
				if field != "between" {
					roles, err = rolesForOperand(field, c.safePeople)
				}
				if err != nil {
					return err
				}
				for _, role := range roles {
					if role == "blind_copy" {
						continue
					}
					add(role, c.compilePersonPredicate(e.Dependency.ID, []string{role}))
				}
			}
		}
		for _, child := range e.Children {
			if err := walk(child, field); err != nil {
				return err
			}
		}
		return nil
	}
	if !c.photoDisplayMetadata {
		if err := walk(resolved.Expression, ""); err != nil {
			return nil, err
		}
		for _, p := range resolved.Query.Filters.People {
			for _, role := range p.Roles {
				if role == "blind_copy" {
					continue
				}
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
	var expressions []string
	var args []any
	var relations compiledQueryRelations
	for _, match := range compiled.roleMatches {
		partial := compiled
		partial.predicate = match.predicate
		predicate, err := partial.bind(generation, nil)
		if err != nil {
			return err
		}
		expressions = append(expressions, "CASE WHEN "+predicate.sql+" THEN 1 ELSE 0 END")
		args = append(args, predicate.args...)
		relations |= predicate.relations
	}
	args = append(args, string(encoded), boolInt(compiled.Query.Filters.CollapseDuplicates))
	population := compiledQueryFragment{sql: `SELECT root.id, ` + strings.Join(expressions, ",") + `
 FROM json_each(?) ids JOIN nodes root ON root.id=ids.value
 JOIN content_versions rootcv ON rootcv.version_id=root.current_version_id
 JOIN content_versions cv ON cv.blob_hash=rootcv.blob_hash
 JOIN nodes n ON n.id=cv.node_id AND n.current_version_id=cv.version_id
 WHERE n.id=root.id OR ?=1`, args: args, relations: relations}
	statement, args, err := bindQueryPopulation(population, coverage, generation)
	if err != nil {
		return err
	}
	if len(statement) > maxCompiledQuerySQL || len(args) > maxCompiledQueryArgs {
		return compileExpressionError(0, len(compiled.Query.Text), "role annotations exceed query bounds")
	}
	found, err := q.QueryContext(ctx, statement, args...)
	if err != nil {
		return fmt.Errorf("annotating roles: %w", err)
	}
	defer func() { _ = found.Close() }()
	for found.Next() {
		var id int64
		values := make([]int, len(compiled.roleMatches))
		dest := make([]any, 0, len(values)+1)
		dest = append(dest, &id)
		for i := range values {
			dest = append(dest, &values[i])
		}
		if err := found.Scan(dest...); err != nil {
			return err
		}
		if i, ok := index[id]; ok {
			for j, value := range values {
				label := compiled.roleMatches[j].label
				if value != 0 && !slices.Contains(rows[i].MatchedRoles, label) {
					rows[i].MatchedRoles = append(rows[i].MatchedRoles, label)
				}
			}
		}
	}
	if err := found.Err(); err != nil {
		return err
	}
	for i := range rows {
		slices.Sort(rows[i].MatchedRoles)
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
	statementSQL := `WITH matched_members AS (` + statement + `)
 SELECT m.node_id,COALESCE(p.person_id,''),COALESCE(p.display_name,''),COALESCE(json_extract(e.value,'$.role'),'')
 FROM matched_members m JOIN nodes n ON n.id=m.node_id
 LEFT JOIN json_each((SELECT json_group_array(json_object('person_id',edge.person_id,'role',edge.role)) FROM ` + currentDocumentPersonEdgesSQL("m.content_version_id", true, hide) + ` edge)) e
 LEFT JOIN persons p ON p.person_id=json_extract(e.value,'$.person_id') AND p.state<>'retired'
 ORDER BY m.node_id,p.person_id,json_extract(e.value,'$.role')`
	rows, err := q.QueryContext(ctx, statementSQL, args...)
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
		err := q.QueryRowContext(ctx, `SELECT person_id,display_name FROM persons WHERE person_id=COALESCE((SELECT surviving_person_id FROM person_aliases WHERE retired_person_id=$1),$2) AND `+personVisibleSQL("persons.person_id", 3), entry.PersonID, entry.PersonID, 1, boolInt(hide == "1")).Scan(&id, &name)
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
