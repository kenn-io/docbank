package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

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
	version, _, err := s.factsVersionTx(ctx, tx, versionID)
	if err != nil {
		return DocumentDateFacts{}, err
	}
	state, view, err := s.freshEventsInTx(ctx, tx, versionID)
	if err != nil {
		return DocumentDateFacts{}, err
	}
	facts := DocumentDateFacts{Version: version, State: state, View: view}
	if err := tx.Commit(); err != nil {
		return DocumentDateFacts{}, err
	}
	return facts, nil
}

// freshEventsInTx checks the current input epoch and decodes the head in the
// caller's snapshot. A retained head is not fresh while its inputs rebuild.
func (s *Store) freshEventsInTx(ctx context.Context, tx *sql.Tx, versionID string) (string, DocumentEventView, error) {
	state, err := effectiveDocumentEventState(ctx, tx, DocumentEventsDeriverFingerprint)
	if err != nil {
		return "", DocumentEventView{}, err
	}
	var attempt, head, fresh, failed bool
	err = tx.QueryRowContext(ctx, `SELECT a.content_version_id IS NOT NULL,h.content_version_id IS NOT NULL,
 COALESCE(`+documentEventFreshIndexedSQL("$2", "$3", "$4")+`,false),
 COALESCE(a.input_epoch=$2 AND a.input_revision=COALESCE(d.revision,0) AND a.state IN ('failed','unavailable'),false)
 FROM content_versions cv LEFT JOIN document_event_dirty d ON d.content_version_id=cv.version_id
 LEFT JOIN document_event_attempts a ON a.content_version_id=cv.version_id
 LEFT JOIN document_event_heads h ON h.content_version_id=cv.version_id
 LEFT JOIN document_event_generations g ON g.generation_id=h.generation_id WHERE cv.version_id=$1`, versionID, state.InputEpoch, state.ContractVersion, state.DeriverFingerprint).Scan(&attempt, &head, &fresh, &failed)
	if err != nil {
		return "", DocumentEventView{}, fmt.Errorf("reading date state: %w", err)
	}
	if fresh {
		view, err := s.documentEventsForVersionTx(ctx, tx, versionID)
		return "published", view, err
	}
	if failed {
		return "failed", DocumentEventView{}, nil
	}
	if !attempt && !head {
		return "none", DocumentEventView{}, nil
	}
	return "pending", DocumentEventView{}, nil
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
	version, hideLocked, err := s.factsVersionTx(ctx, tx, versionID)
	if err != nil {
		return DocumentPeopleFacts{}, err
	}
	eventState, eventView, err := s.freshEventsInTx(ctx, tx, versionID)
	if err != nil {
		return DocumentPeopleFacts{}, err
	}
	facts := DocumentPeopleFacts{Version: version, State: "none", Edges: []document.DocumentPersonEdgeV1{}, Assertions: []PersonDocumentAssertion{}, Candidates: []VersionCandidate{}, Persons: map[string]FactPerson{}}
	edges, head, err := documentPeopleForVersionTx(ctx, tx, versionID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return DocumentPeopleFacts{}, err
	}
	if err != nil {
		if head.ContentVersionID != "" {
			facts.State = "pending"
		}
	} else {
		switch {
		case head.State == documentPeopleStateFailed || head.State == documentPeopleStateUnavailable:
			facts.State = "failed"
		case head.State == documentPeopleStatePublished && head.GenerationID != "" && eventState == "published":
			facts.State = "published"
			facts.Edges = edges
		default:
			facts.State = "pending"
		}
	}
	// Assertions and candidates are current independent evidence, even when the
	// derived publication is unavailable.
	facts.Assertions, err = documentPersonAssertionsTx(ctx, tx, versionID)
	if err != nil {
		return DocumentPeopleFacts{}, err
	}
	if safe {
		facts.Assertions = slices.DeleteFunc(facts.Assertions, func(a PersonDocumentAssertion) bool { return a.Role == "blind_copy" })
	}
	facts.Candidates, err = s.openCandidatesForVersion(ctx, tx, versionID)
	if err != nil {
		return DocumentPeopleFacts{}, err
	}
	if safe {
		// Candidates omit sensitivity. Match each occurrence to the current event
		// generation and fail closed when its authoritative evidence is absent.
		events := eventView.Events.Events
		for i := range facts.Candidates {
			c := &facts.Candidates[i]
			c.SafeLabels = make(map[PersonCandidateOccurrence]string)
			c.Occurrences = slices.DeleteFunc(c.Occurrences, func(o PersonCandidateOccurrence) bool {
				if o.Role == "blind_copy" {
					return true
				}
				found := false
				for _, event := range events {
					for _, actor := range event.Actors {
						kind, id := actor.EvidenceKind, actor.EvidenceID
						if kind == "" {
							kind, id = event.EvidenceKind, event.EvidenceID
						}
						if actor.ActorKey == c.ActorKey && string(actor.Role) == o.Role && string(kind) == o.EvidenceKind && id == o.EvidenceID {
							if event.Sensitive || actor.Sensitive {
								return true
							}
							found = true
							c.SafeLabels[o] = actor.DisplayName
						}
					}
				}
				return !found
			})
		}

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
		if errors.Is(err, ErrNotFound) {
			// Retired persons remain the owners of historical assertions.
			err = tx.QueryRowContext(ctx, `SELECT person_id,display_name,display_name_folded,origin,state,revision,created_at,updated_at FROM persons WHERE person_id=$1 AND state='retired'`, id).Scan(&person.PersonID, &person.DisplayName, &person.DisplayNameFolded, &person.Origin, &person.State, &person.Revision, &person.CreatedAt, &person.UpdatedAt)
			if errors.Is(err, sql.ErrNoRows) {
				return DocumentPeopleFacts{}, ErrDocumentPeopleCorrupt
			}
		}
		if err != nil {
			return DocumentPeopleFacts{}, err
		}
		fp := FactPerson{Person: person, Visible: true, External: []PersonExternalIdentity{}}
		if safe && person.State == "provisional" {
			if err := tx.QueryRowContext(ctx, visiblePersonByIDSQL, id, boolInt(hideLocked)).Scan(&fp.Visible); err != nil {
				return DocumentPeopleFacts{}, err
			}
		}
		identities, err := personExternalIdentitiesTx(ctx, tx, id)
		if err != nil {
			return DocumentPeopleFacts{}, err
		}
		for _, identity := range identities {
			if identity.UIDState == "current" {
				fp.External = append(fp.External, identity)
			}
		}
		facts.Persons[id] = fp
	}
	if err := tx.Commit(); err != nil {
		return DocumentPeopleFacts{}, err
	}
	return facts, nil
}

// factsVersionTx reads the version and applies the caller's Hidden scope once.
func (s *Store) factsVersionTx(ctx context.Context, tx *sql.Tx, versionID string) (ContentVersion, bool, error) {
	version, err := scanContentVersion(tx.QueryRowContext(ctx, `SELECT `+contentVersionCols+` FROM content_versions WHERE version_id=$1`, versionID))
	if err != nil {
		return ContentVersion{}, false, err
	}
	visible, _ := ctx.Value(visiblePersonContextKey{}).(bool)
	if !visible {
		return version, false, nil
	}
	_, err = s.hiddenSession(ctx, tx)
	if err == nil {
		return version, false, nil
	}
	if !errors.Is(err, ErrHiddenLocked) {
		return ContentVersion{}, false, err
	}
	var allowed bool
	if err := tx.QueryRowContext(ctx, `SELECT `+personVersionVisibleSQL("$1"), versionID).Scan(&allowed); err != nil {
		return ContentVersion{}, false, err
	}
	if !allowed {
		return ContentVersion{}, false, ErrNotFound
	}
	return version, true, nil
}
