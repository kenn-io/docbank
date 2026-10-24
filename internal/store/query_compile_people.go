package store

import "go.kenn.io/docbank/document"

// currentPhotoSourcePredicate limits face evidence to a live image or RAW member.
// kind='file' is implied: the nodes CHECK requires a file for a non-null current_version_id.
func currentPhotoSourcePredicate(file, node, version string) string {
	return file + `.role IN ('image','raw') AND ` + node + `.trashed_at IS NULL AND ` + node + `.current_version_id=` + version
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

func personVersionVisibleSQL(version string) string {
	return `NOT EXISTS(SELECT 1 FROM content_versions v JOIN photo_files f ON f.node_id=v.node_id JOIN photo_assets a ON a.asset_id=f.asset_id WHERE v.version_id=` + version + ` AND a.hidden_at IS NOT NULL)`
}
