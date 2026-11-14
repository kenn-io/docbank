package store

import (
	"context"
	"encoding/json/v2"
	"errors"
	"strings"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/query"
)

type safeRoleSearchKey struct{}

// WithSafeRoleSearch excludes blind-copy and sensitive role evidence.
func WithSafeRoleSearch(ctx context.Context) context.Context {
	return context.WithValue(ctx, safeRoleSearchKey{}, true)
}

// currentPhotoSourcePredicate limits face evidence to a live image or RAW member.
// kind='file' is implied: the nodes CHECK requires a file for a non-null current_version_id.
func currentPhotoSourcePredicate(file, node, version string) string {
	return file + `.role IN ('image','raw') AND ` + node + `.trashed_at IS NULL AND ` + node + `.current_version_id=` + version
}

// compileRoleGroup evaluates all selected people in one predicate.
func (c queryCompiler) compileRoleGroup(entries []query.PersonFilter) (compiledQueryFragment, error) {
	fragment := c.compileRolePredicate(entries[0].PersonID, []string{entries[0].Roles[0]})
	person := `COALESCE((SELECT surviving_person_id FROM person_aliases WHERE retired_person_id=?),?)`
	fragment.sql = strings.ReplaceAll(fragment.sql, person, `json_extract(j.value,'$.person_id')`)
	fragment.sql = strings.ReplaceAll(fragment.sql, `role IN (?)`, `role IN (SELECT value FROM json_each(j.value,'$.roles'))`)
	raw, err := json.Marshal(entries)
	if err != nil {
		return compiledQueryFragment{}, err
	}
	fragment.args = append([]any{string(raw)}, fragment.args[6:]...)
	fragment.args = append(fragment.args, document.PersonResolverFingerprint(), c.eventState.InputEpoch, c.eventState.ContractVersion, c.eventState.DeriverFingerprint)
	fragment.sql = `(CASE WHEN NOT EXISTS(SELECT 1 FROM json_each(?) j WHERE (` + fragment.sql + `) IS NOT TRUE) THEN TRUE WHEN EXISTS(SELECT 1 FROM ` + freshPeopleHeadSQL("cv.version_id") + `) THEN FALSE END)`
	return fragment, nil
}

func currentPersonAssertionFence(safe bool) string {
	fence := `a.action='assert'`
	if safe {
		fence += ` AND ` + personVisibleAssertionSQL("a", "1")
	}
	return fence
}

func currentPersonPublishedFence(safe bool) string {
	fence := `h.state='published' AND h.event_generation_id=eh.generation_id AND h.binding_epoch=ps.binding_epoch AND h.node_revision=n.revision AND h.resolver_fingerprint=? AND ` + documentEventFreshIndexedSQL("ea", "ed", "eh", "eg", "?", "?", "?")
	if safe {
		fence += ` AND ` + personVisibleActorSQL("1")
	}
	return fence
}

// currentDocumentPersonEdgesSQL is correlated to the current node and version.
// Its parameters are the resolver fingerprint and current event state.
func currentDocumentPersonEdgesSQL(version string, safe bool) string {
	return `(SELECT COALESCE((SELECT surviving_person_id FROM person_aliases WHERE retired_person_id=a.person_id),a.person_id) AS person_id,a.role FROM person_document_assertions a WHERE a.content_version_id=` + version + ` AND ` + currentPersonAssertionFence(safe) + `
 UNION SELECT COALESCE((SELECT surviving_person_id FROM person_aliases WHERE retired_person_id=p.person_id),p.person_id),p.role FROM document_people p JOIN document_people_heads h ON h.content_version_id=p.content_version_id AND h.generation_id=p.generation_id JOIN document_event_heads eh ON eh.content_version_id=h.content_version_id JOIN document_event_generations eg ON eg.generation_id=eh.generation_id JOIN document_event_attempts ea ON ea.content_version_id=h.content_version_id LEFT JOIN document_event_dirty ed ON ed.content_version_id=h.content_version_id CROSS JOIN document_people_state ps WHERE p.content_version_id=` + version + ` AND ` + currentPersonPublishedFence(safe) + `)`
}

func (c queryCompiler) compilePersonPredicate(id string) compiledQueryFragment {
	person := `COALESCE((SELECT surviving_person_id FROM person_aliases WHERE retired_person_id=?),?)`
	predicate := `(EXISTS(SELECT 1 FROM person_document_assertions a WHERE a.content_version_id=cv.version_id AND a.action='assert' AND a.person_id=` + person + `) OR EXISTS(SELECT 1 FROM document_people p JOIN document_people_heads h ON h.content_version_id=p.content_version_id AND h.generation_id=p.generation_id JOIN document_event_heads eh ON eh.content_version_id=h.content_version_id CROSS JOIN document_people_state ps WHERE p.content_version_id=cv.version_id AND p.person_id=` + person + ` AND h.state='published' AND h.event_generation_id=eh.generation_id AND h.binding_epoch=ps.binding_epoch AND h.node_revision=n.revision AND h.resolver_fingerprint=?))`
	args := []any{id, id, id, id, document.PersonResolverFingerprint()}
	if c.photoDisplayMetadata {
		predicate = `EXISTS(SELECT 1 FROM photo_files member JOIN photo_files pf ON pf.asset_id=member.asset_id JOIN nodes pn ON pn.id=pf.node_id JOIN person_document_assertions a ON ` + currentPhotoSourcePredicate("pf", "pn", "a.content_version_id") + ` WHERE member.node_id=n.id AND a.action='assert' AND a.role='depicted' AND a.person_id=` + person + `)`
		args = []any{id, id}
	}
	return compiledQueryFragment{sql: predicate, args: args}
}

func freshPeopleHeadSQL(version string) string {
	return `document_people_heads h JOIN document_event_heads eh ON eh.content_version_id=h.content_version_id JOIN document_event_generations eg ON eg.generation_id=eh.generation_id JOIN document_event_attempts ea ON ea.content_version_id=h.content_version_id LEFT JOIN document_event_dirty ed ON ed.content_version_id=h.content_version_id CROSS JOIN document_people_state ps WHERE h.content_version_id=` + version + ` AND ` + currentPersonPublishedFence(false)
}

func (c queryCompiler) compileRolePredicate(id string, roles []string) compiledQueryFragment {
	person := `COALESCE((SELECT surviving_person_id FROM person_aliases WHERE retired_person_id=?),?)`
	roleList := strings.TrimSuffix(strings.Repeat("?,", len(roles)), ",")
	assertion := `EXISTS(SELECT 1 FROM person_document_assertions a WHERE a.content_version_id=cv.version_id AND ` + currentPersonAssertionFence(c.safePeople) + ` AND COALESCE((SELECT surviving_person_id FROM person_aliases WHERE retired_person_id=a.person_id),a.person_id)=` + person + ` AND a.role IN (` + roleList + `))`
	edge := `COALESCE((SELECT surviving_person_id FROM person_aliases WHERE retired_person_id=p.person_id),p.person_id)=` + person + ` AND p.role IN (` + roleList + `)`
	if c.safePeople {
		edge += ` AND ` + personVisibleActorSQL("1")
	}
	// One fresh-head scan answers match (1), known absence (0), and unknown (NULL).
	head := strings.Replace(freshPeopleHeadSQL("cv.version_id"), " WHERE h.content_version_id=", " LEFT JOIN document_people p ON p.content_version_id=h.content_version_id AND p.generation_id=h.generation_id WHERE h.content_version_id=", 1)
	args := []any{id, id}
	for _, role := range roles {
		args = append(args, role)
	}
	args = append(args, args...)
	args = append(args, document.PersonResolverFingerprint(), c.eventState.InputEpoch, c.eventState.ContractVersion, c.eventState.DeriverFingerprint)
	return compiledQueryFragment{sql: `(CASE WHEN ` + assertion + ` THEN TRUE ELSE (SELECT MAX(CASE WHEN ` + edge + ` THEN 1 ELSE 0 END) FROM ` + head + `) END)`, args: args}
}

var betweenPersonRoles = func() []string {
	var roles []string
	for _, role := range document.AllEventRoles() {
		if role != "custodian" {
			roles = append(roles, string(role))
		}
	}
	return append(roles, "speaker", "depicted")
}()

func rolesForOperand(field string, safe bool) ([]string, error) {
	switch field {
	case "author", "custodian", "participant", "organizer", "attendee":
		return []string{field}, nil
	case "from":
		return []string{"sender", "organizer"}, nil
	case "to":
		roles := []string{"recipient", "copied", "attendee"}
		if !safe {
			roles = append(roles, "blind_copy")
		}
		return roles, nil
	case "cc":
		return []string{"copied"}, nil
	case "bcc":
		if safe {
			return nil, errors.New("Bcc searches are unavailable here.")
		}
		return []string{"blind_copy"}, nil
	default:
		return nil, errors.New("unknown role operand")
	}
}

func personVersionVisibleSQL(version string) string {
	return `NOT EXISTS(SELECT 1 FROM content_versions visible_version JOIN photo_files visible_file ON visible_file.node_id=visible_version.node_id JOIN photo_assets visible_asset ON visible_asset.asset_id=visible_file.asset_id WHERE visible_version.version_id=` + version + ` AND visible_asset.hidden_at IS NOT NULL)`
}
