package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/docbank/document"

	"go.kenn.io/docbank/internal/query"
)

type compiledRoleMatch struct {
	person  string
	roles   []string
	mention compiledQueryFragment
}

func (c queryCompiler) collectRoleMatches(resolved query.ResolvedQuery) ([]compiledRoleMatch, error) {
	var matches []compiledRoleMatch
	addFilters := func(filters query.Filters) {
		for _, entry := range filters.People {
			matches = append(matches, compiledRoleMatch{person: entry.PersonID, roles: entry.Roles})
		}
	}
	var walk func(*query.ResolvedExpression, string, bool) error
	walk = func(e *query.ResolvedExpression, field string, negative bool) error {
		if e == nil || e.Syntax == nil {
			return nil
		}
		if e.Syntax.Kind == query.ExpressionNot {
			return walk(e.Children[0], field, !negative)
		}
		if e.Syntax.Kind == query.ExpressionField {
			field = e.Syntax.Field
		}
		if e.Saved != nil {
			if !negative {
				addFilters(e.Saved.Query.Filters)
			}
			if err := walk(e.Saved.Expression, "", negative); err != nil {
				return err
			}
		}
		if e.Dependency != nil && !negative {
			if field == "mentions" {
				fragment, err := c.compileExpressionLeaf(e, field)
				if err != nil {
					return err
				}
				matches = append(matches, compiledRoleMatch{mention: fragment})
			} else if slices.Contains([]string{"author", "from", "to", "cc", "bcc", "custodian", "participant", "organizer", "attendee", "between"}, field) {
				roles := betweenPersonRoles
				var err error
				if field != "between" {
					roles, err = rolesForOperand(field, c.safePeople)
				}
				if err != nil {
					return err
				}
				matches = append(matches, compiledRoleMatch{person: e.Dependency.ID, roles: roles})
			}
		}
		for _, child := range e.Children {
			if err := walk(child, field, negative); err != nil {
				return err
			}
		}
		return nil
	}
	if !c.photoDisplayMetadata {
		if err := walk(resolved.Expression, "", false); err != nil {
			return nil, err
		}
		addFilters(resolved.Query.Filters)
	}
	return matches, nil
}

func annotateMatchedRoles(ctx context.Context, q metadataQuerier, compiled CompiledQuery, generation string, coverage CoverageSelection, rows []SnapshotRow) error {
	if len(rows) == 0 {
		return nil
	}
	roots := make(map[int64][]int)
	if !compiled.Query.Filters.CollapseDuplicates {
		for i, row := range rows {
			roots[row.NodeID] = []int{i}
		}
	} else {
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
		rowIndex := make(map[int64]int, len(rows))
		for i, row := range rows {
			rowIndex[row.NodeID] = i
		}
		encoded, err := json.Marshal(ids)
		if err != nil {
			return err
		}
		population := compiledQueryFragment{sql: `SELECT root.id,n.id
 FROM json_each(?) ids JOIN nodes root ON root.id=ids.value
 JOIN content_versions rootcv ON rootcv.version_id=root.current_version_id
 JOIN content_versions cv ON cv.blob_hash=rootcv.blob_hash
 JOIN nodes n ON n.id=cv.node_id AND n.current_version_id=cv.version_id
 JOIN (` + members.sql + `) matched ON matched.node_id=n.id AND matched.content_version_id=cv.version_id
 WHERE 1=1`, args: append([]any{string(encoded)}, members.args...), relations: members.relations}
		statement, args, err := bindQueryPopulation(population, coverage, generation)
		if err != nil {
			return err
		}
		err = func() (err error) {
			found, err := q.QueryContext(ctx, statement, args...)
			if err != nil {
				return fmt.Errorf("loading matched role members: %w", err)
			}
			defer func() {
				if closeErr := found.Close(); err == nil {
					err = closeErr
				}
			}()
			for found.Next() {
				var root, member int64
				if err := found.Scan(&root, &member); err != nil {
					return err
				}
				roots[member] = append(roots[member], rowIndex[root])
			}
			return found.Err()
		}()
		if err != nil {
			return err
		}
	}

	ids := make([]int64, 0, len(roots))
	for id := range roots {
		ids = append(ids, id)
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	args := []any{string(encoded), document.PersonResolverFingerprint(), compiled.eventState.InputEpoch, compiled.eventState.ContractVersion, compiled.eventState.DeriverFingerprint}
	found, err := q.QueryContext(ctx, `SELECT n.id,json_extract(e.value,'$.person_id'),json_extract(e.value,'$.role') FROM json_each(?) ids JOIN nodes n ON n.id=ids.value JOIN content_versions cv ON cv.version_id=n.current_version_id JOIN json_each((SELECT json_group_array(json_object('person_id',edge.person_id,'role',edge.role)) FROM `+currentDocumentPersonEdgesSQL("cv.version_id", compiled.safePeople, "1")+` edge)) e`, args...)
	if err != nil {
		return fmt.Errorf("loading matched roles: %w", err)
	}
	for found.Next() {
		var node int64
		var person, role string
		if err := found.Scan(&node, &person, &role); err != nil {
			_ = found.Close()
			return err
		}
		for _, match := range compiled.roleMatches {
			if match.person != person || !slices.Contains(match.roles, role) {
				continue
			}
			for _, i := range roots[node] {
				if !slices.Contains(rows[i].MatchedRoles, role) {
					rows[i].MatchedRoles = append(rows[i].MatchedRoles, role)
				}
			}
		}
	}
	if err := found.Err(); err != nil {
		_ = found.Close()
		return err
	}
	if err := found.Close(); err != nil {
		return err
	}
	for _, match := range compiled.roleMatches {
		if match.mention.sql == "" {
			continue
		}
		partial := compiled
		partial.predicate = match.mention
		predicate, err := partial.bind(generation, nil)
		if err != nil {
			return err
		}
		selection := compiledQueryFragment{sql: `SELECT n.id FROM json_each(?) ids JOIN nodes n ON n.id=ids.value JOIN content_versions cv ON cv.version_id=n.current_version_id WHERE ` + predicate.sql, args: append([]any{string(encoded)}, predicate.args...), relations: predicate.relations}
		statement, args, err := bindQueryPopulation(selection, coverage, generation)
		if err != nil {
			return err
		}
		found, err := q.QueryContext(ctx, statement, args...)
		if err != nil {
			return err
		}
		for found.Next() {
			var node int64
			if err := found.Scan(&node); err != nil {
				_ = found.Close()
				return err
			}
			for _, i := range roots[node] {
				if !slices.Contains(rows[i].MatchedRoles, "mentioned") {
					rows[i].MatchedRoles = append(rows[i].MatchedRoles, "mentioned")
				}
			}
		}
		if err := found.Err(); err != nil {
			_ = found.Close()
			return err
		}
		if err := found.Close(); err != nil {
			return err
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
	args = append(args, document.PersonResolverFingerprint(), scoped.eventState.InputEpoch, scoped.eventState.ContractVersion, scoped.eventState.DeriverFingerprint, document.PersonResolverFingerprint(), scoped.eventState.InputEpoch, scoped.eventState.ContractVersion, scoped.eventState.DeriverFingerprint)
	statementSQL := `WITH matched_members AS (` + statement + `)
 SELECT m.node_id,COALESCE(p.person_id,''),COALESCE(p.display_name,''),COALESCE(json_extract(e.value,'$.role'),''),EXISTS(SELECT 1 FROM ` + freshPeopleHeadSQL("m.content_version_id") + `)
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
		var current bool
		if err := rows.Scan(&node, &id, &name, &role, &current); err != nil {
			return SnapshotFacet{}, err
		}
		if !current {
			return unavailableSnapshotFacet("person_role", "people_evidence_pending"), nil
		}
		observation.add("person_role", node, id, name, 1, memberLimit)
		if observation.limited {
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
