package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/docbank/document"
)

// DocumentDateFacts is a version and its current derived events, read together.
type DocumentDateFacts struct {
	Version ContentVersion
	State   string
	View    DocumentEventView
}

func (s *Store) DocumentDateFacts(ctx context.Context, versionID string) (DocumentDateFacts, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return DocumentDateFacts{}, err
	}
	defer func() { _ = tx.Rollback() }()
	version, err := scanContentVersion(tx.QueryRowContext(ctx, `SELECT `+contentVersionCols+` FROM content_versions WHERE version_id=$1`, versionID))
	if err != nil {
		return DocumentDateFacts{}, err
	}
	state, err := effectiveDocumentEventState(ctx, tx, DocumentEventsDeriverFingerprint)
	if err != nil {
		return DocumentDateFacts{}, err
	}
	var attempt, head, fresh, failed bool
	err = tx.QueryRowContext(ctx, `SELECT a.content_version_id IS NOT NULL,h.content_version_id IS NOT NULL,
 COALESCE(a.input_epoch=$2 AND a.input_revision=COALESCE(d.revision,0) AND a.state='indexed' AND h.input_epoch=a.input_epoch AND g.inputs_sha256=a.inputs_sha256 AND g.contract_version=$3 AND g.deriver_fingerprint=$4,false),
 COALESCE(a.input_epoch=$2 AND a.input_revision=COALESCE(d.revision,0) AND a.state IN ('failed','unavailable'),false)
 FROM content_versions cv LEFT JOIN document_event_dirty d ON d.content_version_id=cv.version_id
 LEFT JOIN document_event_attempts a ON a.content_version_id=cv.version_id
 LEFT JOIN document_event_heads h ON h.content_version_id=cv.version_id
 LEFT JOIN document_event_generations g ON g.generation_id=h.generation_id WHERE cv.version_id=$1`, versionID, state.InputEpoch, state.ContractVersion, state.DeriverFingerprint).Scan(&attempt, &head, &fresh, &failed)
	if err != nil {
		return DocumentDateFacts{}, fmt.Errorf("reading date state: %w", err)
	}
	facts := DocumentDateFacts{Version: version, State: "pending"}
	switch {
	case fresh:
		facts.State = "published"
		facts.View, err = s.documentEventsForVersionTx(ctx, tx, versionID)
		if err != nil {
			return DocumentDateFacts{}, err
		}
	case failed:
		facts.State = "failed"
	case !attempt && !head:
		facts.State = "none"
	}
	if err := tx.Commit(); err != nil {
		return DocumentDateFacts{}, err
	}
	return facts, nil
}

// DocumentPeopleFacts is an attribution snapshot with version-scoped evidence.
type DocumentPeopleFacts struct {
	Version    ContentVersion
	State      string
	Edges      []document.DocumentPersonEdgeV1
	Assertions []PersonDocumentAssertion
	Candidates []VersionCandidate
	Persons    map[string]FactPerson
}

type FactPerson struct {
	Person   Person
	External []PersonExternalIdentity
	Visible  bool
}

func (s *Store) DocumentPeopleFacts(ctx context.Context, versionID string, safe bool) (DocumentPeopleFacts, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return DocumentPeopleFacts{}, err
	}
	defer func() { _ = tx.Rollback() }()
	version, err := scanContentVersion(tx.QueryRowContext(ctx, `SELECT `+contentVersionCols+` FROM content_versions WHERE version_id=$1`, versionID))
	if err != nil {
		return DocumentPeopleFacts{}, err
	}
	facts := DocumentPeopleFacts{Version: version, State: "none", Edges: []document.DocumentPersonEdgeV1{}, Assertions: []PersonDocumentAssertion{}, Candidates: []VersionCandidate{}, Persons: map[string]FactPerson{}}
	head, err := documentPeopleHeadTx(ctx, tx, versionID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return DocumentPeopleFacts{}, err
	}
	if err == nil {
		var current bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM content_versions cv JOIN nodes n ON n.id=cv.node_id JOIN document_event_heads eh ON eh.content_version_id=cv.version_id CROSS JOIN document_people_state ps WHERE cv.version_id=$1 AND eh.generation_id=$2 AND ps.binding_epoch=$3 AND n.revision=$4)`, versionID, head.EventGenerationID, head.BindingEpoch, head.NodeRevision).Scan(&current)
		if err != nil {
			return DocumentPeopleFacts{}, err
		}
		switch {
		case !current:
			facts.State = "pending"
		case head.State == documentPeopleStateFailed || head.State == documentPeopleStateUnavailable:
			facts.State = "failed"
		case head.State == documentPeopleStatePublished && head.GenerationID != "":
			facts.State = "published"
			var raw []byte
			var checksum string
			if err := tx.QueryRowContext(ctx, `SELECT canonical_json,checksum FROM document_people_generations WHERE generation_id=$1`, head.GenerationID).Scan(&raw, &checksum); err != nil {
				return DocumentPeopleFacts{}, ErrDocumentPeopleCorrupt
			}
			record, actual, err := document.DecodeDocumentPeopleV1(raw)
			if err != nil || actual != checksum || record.ContentVersionID != versionID || record.EventGenerationID != head.EventGenerationID || int64(len(record.Edges)) != head.EdgeCount {
				return DocumentPeopleFacts{}, ErrDocumentPeopleCorrupt
			}
			facts.Edges = record.Edges
		default:
			facts.State = "pending"
		}
	}
	// Assertions and candidates are current independent evidence, even when the
	// derived publication is unavailable.
	rows, err := tx.QueryContext(ctx, `SELECT assertion_id,content_version_id,person_id,role,action,note,recorded_at,revision,photo_mark FROM person_document_assertions WHERE content_version_id=$1 ORDER BY role,person_id,assertion_id`, versionID)
	if err != nil {
		return DocumentPeopleFacts{}, err
	}
	for rows.Next() {
		a, err := scanMarkedPersonAssertion(rows)
		if err != nil {
			_ = rows.Close()
			return DocumentPeopleFacts{}, err
		}
		if !safe || a.Role != "blind_copy" {
			facts.Assertions = append(facts.Assertions, a)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return DocumentPeopleFacts{}, err
	}
	_ = rows.Close()
	facts.Candidates, err = openCandidatesForVersion(ctx, tx, versionID)
	if err != nil {
		return DocumentPeopleFacts{}, err
	}
	ids := map[string]bool{}
	for _, e := range facts.Edges {
		if !safe || (!e.Sensitive && e.Role != "blind_copy") {
			ids[e.PersonID] = true
		}
	}
	for _, a := range facts.Assertions {
		ids[a.PersonID] = true
	}
	for id := range ids {
		person, _, err := personByIDTx(ctx, tx, id)
		if err != nil {
			return DocumentPeopleFacts{}, err
		}
		fp := FactPerson{Person: person, Visible: true, External: []PersonExternalIdentity{}}
		if safe && person.State == "provisional" {
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM person_document_assertions a WHERE a.person_id=$1 AND `+personVisibleAssertionSQL("a", "$2")+`) OR EXISTS(SELECT 1 FROM document_people p WHERE p.person_id=$1 AND `+personVisibleActorSQL("p", "$2")+`)`, id, 1).Scan(&fp.Visible); err != nil {
				return DocumentPeopleFacts{}, err
			}
		}
		rows, err := tx.QueryContext(ctx, `SELECT system,archive_id,uid FROM person_external_identities WHERE person_id=$1 AND uid_state='current' ORDER BY system,archive_id,uid`, id)
		if err != nil {
			return DocumentPeopleFacts{}, err
		}
		for rows.Next() {
			var x PersonExternalIdentity
			if err := rows.Scan(&x.System, &x.ArchiveID, &x.UID); err != nil {
				_ = rows.Close()
				return DocumentPeopleFacts{}, err
			}
			fp.External = append(fp.External, x)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return DocumentPeopleFacts{}, err
		}
		_ = rows.Close()
		facts.Persons[id] = fp
	}
	if err := tx.Commit(); err != nil {
		return DocumentPeopleFacts{}, err
	}
	return facts, nil
}
