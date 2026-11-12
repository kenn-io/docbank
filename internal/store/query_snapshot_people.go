package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"

	"go.kenn.io/docbank/document"

	"go.kenn.io/docbank/internal/query"
)

type compiledRoleMatch struct {
	label     string
	predicate compiledQueryFragment
	gates     []compiledQueryFragment
}

func (c queryCompiler) collectRoleMatches(resolved query.ResolvedQuery) ([]compiledRoleMatch, error) {
	var matches []compiledRoleMatch
	add := func(label string, fragment compiledQueryFragment, gates []compiledQueryFragment) {
		matches = append(matches, compiledRoleMatch{label: label, predicate: fragment, gates: slices.Clone(gates)})
	}
	var walk func(*query.ResolvedExpression, string, []compiledQueryFragment) error
	walk = func(e *query.ResolvedExpression, field string, gates []compiledQueryFragment) error {
		if e == nil || e.Syntax == nil {
			return nil
		}
		if e.Syntax.Kind == query.ExpressionNot {
			return nil
		}
		if e.Syntax.Kind == query.ExpressionField {
			field = e.Syntax.Field
		}
		if e.Syntax.Kind == query.ExpressionAnd || e.Syntax.Kind == query.ExpressionField && field == "between" {
			branch, err := c.compileResolvedExpression(e, field)
			if err != nil {
				return err
			}
			gates = append(slices.Clone(gates), branch)
		}
		if e.Saved != nil {
			branch, err := c.compileSavedPredicate(e)
			if err != nil {
				return err
			}
			gates = append(slices.Clone(gates), branch)
			for _, p := range e.Saved.Query.Filters.People {
				for _, role := range p.Roles {
					if c.safePeople && role == "blind_copy" {
						continue
					}
					add(role, c.compilePersonPredicate(p.PersonID, []string{role}), gates)
				}
			}
			if err := walk(e.Saved.Expression, "", gates); err != nil {
				return err
			}
		}
		if e.Dependency != nil {
			if field == "mentions" {
				f, err := c.compileExpressionLeaf(e, field)
				if err != nil {
					return err
				}
				add("mentioned", f, gates)
			} else if slices.Contains([]string{"author", "from", "to", "cc", "bcc", "custodian", "participant", "organizer", "attendee", "between"}, field) {
				roles := betweenPersonRoles
				var err error
				if field != "between" {
					roles, err = rolesForOperand(field, c.safePeople)
				}
				if err != nil {
					return err
				}
				for _, role := range roles {
					if c.safePeople && role == "blind_copy" {
						continue
					}
					add(role, c.compilePersonPredicate(e.Dependency.ID, []string{role}), gates)
				}
			}
		}
		for _, child := range e.Children {
			// The between field's synthetic AND is already compiled above.
			if field == "between" && e.Syntax.Kind == query.ExpressionField {
				for _, operand := range child.Children {
					if err := walk(operand, field, gates); err != nil {
						return err
					}
				}
				continue
			}
			if err := walk(child, field, gates); err != nil {
				return err
			}
		}
		return nil
	}
	if !c.photoDisplayMetadata {
		if err := walk(resolved.Expression, "", nil); err != nil {
			return nil, err
		}
		for _, p := range resolved.Query.Filters.People {
			for _, role := range p.Roles {
				if c.safePeople && role == "blind_copy" {
					continue
				}
				add(role, c.compilePersonPredicate(p.PersonID, []string{role}), nil)
			}
		}
	}
	// Combine only alternatives with identical gates. Keep headroom for the
	// live-member predicate and statement bindings, and split large groups.
	var grouped []compiledRoleMatch
	for _, match := range matches {
		combined := false
		for i := range grouped {
			group := &grouped[i]
			if group.label != match.label || !reflect.DeepEqual(group.gates, match.gates) {
				continue
			}
			predicate := joinCompiledFragments([]compiledQueryFragment{group.predicate, match.predicate}, ` OR `)
			if len(predicate.sql) > 200<<10 || len(predicate.args) > 3000 {
				continue
			}
			group.predicate = predicate
			combined = true
			break
		}
		if !combined {
			grouped = append(grouped, match)
		}
	}
	return grouped, nil
}

func annotateMatchedRoles(ctx context.Context, q metadataQuerier, compiled CompiledQuery, generation string, coverage CoverageSelection, rows []SnapshotRow) error {
	if len(rows) == 0 {
		return nil
	}
	ids := make([]int64, len(rows))
	for i, row := range rows {
		ids[i] = row.NodeID
	}
	allMembers := compiled
	allMembers.Query.Filters.CollapseDuplicates = false
	members, err := matchedPopulation(allMembers, generation, nil)
	if err != nil {
		return err
	}
	roots := make(map[int64][]int)
	rowIndex := make(map[int64]int, len(rows))
	for i, row := range rows {
		rowIndex[row.NodeID] = i
	}
	// IDs come from snapshot rows, not query text. Chunk their JSON literals so
	// they consume no bind slots and add only bounded SQL to the predicate.
	const memberBatch = 128
	for batch := range slices.Chunk(ids, memberBatch) {
		encoded, err := json.Marshal(batch)
		if err != nil {
			return err
		}
		population := compiledQueryFragment{sql: `SELECT root.id,n.id
 FROM json_each('` + string(encoded) + `') ids JOIN nodes root ON root.id=ids.value
 JOIN content_versions rootcv ON rootcv.version_id=root.current_version_id
 JOIN content_versions cv ON cv.blob_hash=rootcv.blob_hash
 JOIN nodes n ON n.id=cv.node_id AND n.current_version_id=cv.version_id
 JOIN (` + members.sql + `) matched ON matched.node_id=n.id AND matched.content_version_id=cv.version_id
 WHERE n.id=root.id OR ` + fmt.Sprint(boolInt(compiled.Query.Filters.CollapseDuplicates)) + `=1`, args: members.args, relations: members.relations}
		statement, args, err := bindQueryPopulation(population, coverage, generation)
		if err != nil {
			return err
		}
		found, err := q.QueryContext(ctx, statement, args...)
		if err != nil {
			return fmt.Errorf("loading matched role members: %w", err)
		}
		for found.Next() {
			var root, member int64
			if err := found.Scan(&root, &member); err != nil {
				_ = found.Close()
				return err
			}
			roots[member] = append(roots[member], rowIndex[root])
		}
		err = found.Err()
		if closeErr := found.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
	}

	// Each gate and role predicate is the existing compiler's SQL. Intersecting
	// their matching members keeps nested branch attribution without repeating
	// a whole branch or the population in one amplified statement.
	for _, match := range compiled.roleMatches {
		candidates := make([]int64, 0, len(roots))
		for id := range roots {
			candidates = append(candidates, id)
		}
		for _, fragment := range append(slices.Clone(match.gates), match.predicate) {
			if len(candidates) == 0 {
				break
			}
			partial := compiled
			partial.predicate = fragment
			predicate, err := partial.bind(generation, nil)
			if err != nil {
				return err
			}
			var selected []int64
			for batch := range slices.Chunk(candidates, memberBatch) {
				encoded, err := json.Marshal(batch)
				if err != nil {
					return err
				}
				selection := compiledQueryFragment{sql: `SELECT n.id FROM json_each('` + string(encoded) + `') ids JOIN nodes n ON n.id=ids.value JOIN content_versions cv ON cv.version_id=n.current_version_id WHERE ` + predicate.sql, args: predicate.args, relations: predicate.relations}
				statement, args, err := bindQueryPopulation(selection, coverage, generation)
				if err != nil {
					return err
				}
				found, err := q.QueryContext(ctx, statement, args...)
				if err != nil {
					return fmt.Errorf("annotating roles: %w", err)
				}
				for found.Next() {
					var id int64
					if err := found.Scan(&id); err != nil {
						_ = found.Close()
						return err
					}
					selected = append(selected, id)
				}
				err = found.Err()
				if closeErr := found.Close(); err == nil {
					err = closeErr
				}
				if err != nil {
					return err
				}
			}
			candidates = selected
		}
		for _, member := range candidates {
			for _, i := range roots[member] {
				if !slices.Contains(rows[i].MatchedRoles, match.label) {
					rows[i].MatchedRoles = append(rows[i].MatchedRoles, match.label)
				}
			}
		}
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
	observation := newSnapshotFacetAccumulator()
	roles := map[string]map[string]bool{}
	for rows.Next() {
		var node, id, name, role string
		if err := rows.Scan(&node, &id, &name, &role); err != nil {
			return SnapshotFacet{}, err
		}
		observation.add("person_role", node, id, name, 1, math.MaxInt64)
		if observation.total > memberLimit {
			return unavailableSnapshotFacet("person_role", "member_budget_exceeded"), nil
		}
		if id == "" {
			continue
		}
		if roles[id] == nil {
			roles[id] = map[string]bool{}
		}
		roles[id][role] = true
	}
	if err := rows.Err(); err != nil {
		return SnapshotFacet{}, err
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
		observation.labels[id] = name
	}
	out, other := finalizeSnapshotFacetValues(observation.counts, observation.labels, selected, false, 20)
	for i := range out {
		for role := range roles[out[i].Key] {
			out[i].Roles = append(out[i].Roles, role)
		}
		slices.Sort(out[i].Roles)
	}
	return SnapshotFacet{Dimension: "person_role", Available: true, Total: &observation.total, Values: out, Missing: &observation.missing, Other: &other}, nil
}
