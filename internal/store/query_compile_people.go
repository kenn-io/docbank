package store

import (
	"errors"
	"strings"

	"go.kenn.io/docbank/document"
)

// currentPhotoSourcePredicate limits face evidence to a live image or RAW member.
func currentPhotoSourcePredicate(file, node, version string) string {
	return file + `.role IN ('image','raw') AND ` + node + `.trashed_at IS NULL AND ` + node + `.current_version_id=` + version
}

func currentPersonAssertionFence(safe bool, hideLocked string) string {
	fence := `a.action='assert'`
	if safe {
		fence += ` AND ` + personVisibleAssertionSQL("a", hideLocked)
	}
	return fence
}

func currentPersonPublishedFence(safe bool, hideLocked string) string {
	fence := `h.state='published' AND h.event_generation_id=eh.generation_id AND h.binding_epoch=ps.binding_epoch AND h.node_revision=n.revision AND h.resolver_fingerprint=? AND ` + documentEventFreshIndexedSQL("ea", "ed", "eh", "eg", "?", "?", "?")
	if safe {
		fence += ` AND ` + personVisibleActorSQL("p", hideLocked)
	}
	return fence
}

// currentDocumentPersonEdgesSQL is correlated to the current node and version.
// Its parameters are the resolver fingerprint and current event state.
func currentDocumentPersonEdgesSQL(version string, safe bool, hideLocked string) string {
	return `(SELECT COALESCE((SELECT surviving_person_id FROM person_aliases WHERE retired_person_id=a.person_id),a.person_id) AS person_id,a.role FROM person_document_assertions a WHERE a.content_version_id=` + version + ` AND ` + currentPersonAssertionFence(safe, hideLocked) + `
 UNION SELECT COALESCE((SELECT surviving_person_id FROM person_aliases WHERE retired_person_id=p.person_id),p.person_id),p.role FROM document_people p JOIN document_people_heads h ON h.content_version_id=p.content_version_id AND h.generation_id=p.generation_id JOIN document_event_heads eh ON eh.content_version_id=h.content_version_id JOIN document_event_generations eg ON eg.generation_id=eh.generation_id JOIN document_event_attempts ea ON ea.content_version_id=h.content_version_id LEFT JOIN document_event_dirty ed ON ed.content_version_id=h.content_version_id CROSS JOIN document_people_state ps WHERE p.content_version_id=` + version + ` AND ` + currentPersonPublishedFence(safe, hideLocked) + `)`
}

func (c queryCompiler) compilePersonPredicate(id string, roles []string) compiledQueryFragment {
	person := `COALESCE((SELECT surviving_person_id FROM person_aliases WHERE retired_person_id=?),?)`
	if c.photoDisplayMetadata {
		predicate := `EXISTS(SELECT 1 FROM photo_files member JOIN photo_files pf ON pf.asset_id=member.asset_id JOIN nodes pn ON pn.id=pf.node_id JOIN person_document_assertions a ON ` + currentPhotoSourcePredicate("pf", "pn", "a.content_version_id") + ` WHERE member.node_id=n.id AND a.action='assert' AND a.role='depicted' AND a.person_id=` + person
		return compiledQueryFragment{sql: predicate + `)`, args: []any{id, id}}
	}
	edges := currentDocumentPersonEdgesSQL("cv.version_id", c.safePeople && len(roles) > 0, c.hideLocked)
	predicate := `EXISTS(SELECT 1 FROM ` + edges + ` edge WHERE edge.person_id=` + person
	args := []any{document.PersonResolverFingerprint(), c.eventState.InputEpoch, c.eventState.ContractVersion, c.eventState.DeriverFingerprint, id, id}
	if len(roles) > 0 {
		predicate += ` AND edge.role IN (` + strings.TrimSuffix(strings.Repeat("?,", len(roles)), ",") + `)`
		for _, role := range roles {
			args = append(args, role)
		}
	}
	return compiledQueryFragment{sql: predicate + `)`, args: args, personEdges: true}
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
			return nil, errors.New("bcc: is not available with safe people disclosure")
		}
		return []string{"blind_copy"}, nil
	default:
		return nil, errors.New("unknown role operand")
	}
}

func personVersionVisibleSQL(version string) string {
	return `NOT EXISTS(SELECT 1 FROM content_versions visible_version JOIN photo_files visible_file ON visible_file.node_id=visible_version.node_id JOIN photo_assets visible_asset ON visible_asset.asset_id=visible_file.asset_id WHERE visible_version.version_id=` + version + ` AND visible_asset.hidden_at IS NOT NULL)`
}
