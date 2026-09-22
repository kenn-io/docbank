package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/canonical"
)

const (
	maxDocumentPeopleInputsBytes   = 8 << 20
	documentPeopleStatePublished   = "published"
	documentPeopleStateFailed      = ExtractionFailed
	documentPeopleStateUnavailable = "unavailable"
)

var (
	ErrPeopleInputsChanged   = errors.New("people resolver inputs changed")
	ErrPeopleInputsTooLarge  = errors.New("people resolver inputs exceed the encoded byte limit")
	ErrDocumentPeopleCorrupt = errors.New("document people generation does not match its recorded evidence")
)

type DocumentPeopleTarget struct {
	ContentVersionID string
	NodeID           int64
	Reason           string
}

// DocumentPeopleActor carries the retained evidence needed for person resolution.
// An actor with independent, undated evidence has no axis key.
type DocumentPeopleActor struct {
	Role, ActorKey, DisplayName, Address string
	EvidenceKind, EvidenceID, AxisKey    string
	Sensitive                            bool
}

// DisplayLabel bounds the derived label while leaving the retained evidence intact.
func (claim DocumentPeopleActor) DisplayLabel() string {
	label := strings.TrimSpace(claim.DisplayName)
	if label == "" {
		label = strings.TrimSpace(claim.Address)
	}
	if label == "" {
		_, label, _ = strings.Cut(claim.ActorKey, ":")
	}
	if len(label) > document.MaxPersonDisplayNameBytes {
		end := document.MaxPersonDisplayNameBytes
		for !utf8.RuneStart(label[end]) {
			end--
		}
		label = label[:end]
	}
	return label
}

type DocumentPeopleInputs struct {
	ContentVersionID  string
	EventGenerationID string
	NodeID            int64
	NodeRevision      int64
	BindingEpoch      int64
	CandidateOverflow int64
	Actors            []DocumentPeopleActor
	Bindings          map[string][]Person
	Custodians        []CustodianAssignment
	Assertions        []PersonDocumentAssertion
	Persons           map[string]Person
}

type DocumentPeopleResolution struct {
	UnresolvedActors  int64
	SuppressedActors  int64
	CandidateOverflow int64
	OverLimit         bool
}

type DocumentPeoplePublication struct {
	People              document.DocumentPeopleV1
	Resolution          DocumentPeopleResolution
	InputsSHA256        string
	ResolverFingerprint string
	NodeID              int64
	NodeRevision        int64
	BindingEpoch        int64
}

type DocumentPeopleHead struct {
	ContentVersionID, EventGenerationID, GenerationID, InputsSHA256, ResolverFingerprint string
	BindingEpoch, NodeRevision, EdgeCount                                                int64
	State, FailureReason, PublishedAt                                                    string
}

func documentPeopleGenerationID(p DocumentPeoplePublication, resultChecksum string) (string, error) {
	raw, err := canonical.Marshal([]string{p.People.ContractVersion, p.People.ContentVersionID,
		p.People.EventGenerationID, p.InputsSHA256, p.ResolverFingerprint, resultChecksum})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// PrepareDocumentPeopleInputs provisions exact identities and retains review
// candidates in the same snapshot used for resolution. Failed loads return no
// inputs, except a size limit returns the fence needed to record unavailable coverage.
func (s *Store) PrepareDocumentPeopleInputs(ctx context.Context, versionID string) (DocumentPeopleInputs, error) {
	input := DocumentPeopleInputs{ContentVersionID: versionID,
		Actors: []DocumentPeopleActor{}, Bindings: map[string][]Person{},
		Custodians: []CustodianAssignment{}, Assertions: []PersonDocumentAssertion{}, Persons: map[string]Person{}}
	if err := validateUUIDv4(versionID); err != nil {
		return DocumentPeopleInputs{}, fmt.Errorf("invalid content version ID: %w", err)
	}
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		audited, err := auditAuthorityActiveTx(ctx, tx)
		if err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT cv.node_id,n.revision,h.generation_id FROM content_versions cv
			JOIN nodes n ON n.id=cv.node_id
			JOIN document_event_heads h ON h.content_version_id=cv.version_id WHERE cv.version_id=?`, versionID).
			Scan(&input.NodeID, &input.NodeRevision, &input.EventGenerationID); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT binding_epoch FROM document_people_state WHERE singleton=1`).Scan(&input.BindingEpoch); err != nil {
			return err
		}
		if err := checkDocumentPeopleInputRowBoundsTx(ctx, tx, versionID); err != nil {
			return err
		}
		actors, err := documentEventActorClaimsTx(ctx, tx, input.EventGenerationID)
		if err != nil {
			return err
		}
		input.Actors = actors
		claimsByKey := make(map[string][]DocumentPeopleActor, len(actors))
		orderedKeys := make([]string, 0, len(actors))
		for _, claim := range actors {
			if _, seen := claimsByKey[claim.ActorKey]; !seen {
				orderedKeys = append(orderedKeys, claim.ActorKey)
			}
			claimsByKey[claim.ActorKey] = append(claimsByKey[claim.ActorKey], claim)
		}
		for _, actorKey := range orderedKeys {
			if actorKey == "" {
				continue
			}
			claims := claimsByKey[actorKey]
			claim := claims[0]
			matches, err := peopleBoundToActorKeyTx(ctx, tx, actorKey)
			if err != nil {
				return err
			}
			// Audited rebuilds may read authority, but cannot create it without an audit record.
			if !audited {
				if len(matches) == 0 && actorKeyAutoProvisionEligible(claim.ActorKey) {
					// Provisioning an unbound exact key cannot change prior resolutions.
					// Only authority edits need to advance the global binding epoch.
					person, err := provisionActorPersonTx(ctx, tx, claim)
					if err != nil {
						return err
					}
					matches = []Person{person}
				} else if len(matches) != 1 || strings.HasPrefix(claim.ActorKey, "name_alias:") {
					retained, err := s.openActorCandidatesTx(ctx, tx, versionID, claims, matches)
					if err != nil {
						return err
					}
					if !retained {
						input.CandidateOverflow++
					}
				}
			}
			if strings.HasPrefix(actorKey, "name_alias:") {
				matches = nil
			}
			input.Bindings[actorKey] = matches
		}
		if err := loadDocumentPeopleCustodiansTx(ctx, tx, versionID, &input); err != nil {
			return err
		}
		if err := loadDocumentPeopleAssertionsTx(ctx, tx, versionID, &input); err != nil {
			return err
		}
		if err := loadReferencedPeopleTx(ctx, tx, &input); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		if !errors.Is(err, ErrPeopleInputsTooLarge) {
			input = DocumentPeopleInputs{}
		}
		return input, fmt.Errorf("loading document people inputs for %s: %w", versionID, err)
	}
	raw, err := canonical.Marshal(input)
	if err != nil {
		return DocumentPeopleInputs{}, fmt.Errorf("encoding document people inputs: %w", err)
	}
	if len(raw) > maxDocumentPeopleInputsBytes {
		return input, ErrPeopleInputsTooLarge
	}
	return input, nil
}

func checkDocumentPeopleInputRowBoundsTx(ctx context.Context, tx *sql.Tx, versionID string) error {
	const maxRows = document.MaxPersonEdgesPerVersion + 1
	var custodians int64
	err := tx.QueryRowContext(ctx, `WITH `+CollectionMembershipCTE+`
		SELECT count(*) FROM custodian_assignments ca WHERE ca.retired_at IS NULL AND (
		(ca.scope_kind='document' AND ca.content_version_id=?) OR
		(ca.scope_kind='collection' AND EXISTS(SELECT 1 FROM collection_members cm JOIN nodes n ON n.id=cm.node_id WHERE cm.ingest_id=ca.ingest_id AND n.current_version_id=?)))`,
		versionID, versionID).Scan(&custodians)
	if err != nil {
		return err
	}
	var assertions int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM person_document_assertions WHERE content_version_id=?`, versionID).Scan(&assertions); err != nil {
		return err
	}
	if custodians > maxRows || assertions > maxRows {
		return ErrPeopleInputsTooLarge
	}
	return nil
}

func documentEventActorClaimsTx(ctx context.Context, tx *sql.Tx, generationID string) ([]DocumentPeopleActor, error) {
	rows, err := tx.QueryContext(ctx, `SELECT a.role,a.actor_key,a.display_name,a.address,
		CASE WHEN a.evidence_kind='' THEN e.evidence_kind ELSE a.evidence_kind END,
		CASE WHEN a.evidence_id='' THEN e.evidence_id ELSE a.evidence_id END,
		CASE WHEN a.evidence_kind='' OR (a.evidence_kind=e.evidence_kind AND a.evidence_id=e.evidence_id) THEN e.axis_key ELSE '' END,a.sensitive
		FROM document_event_actors a JOIN document_events e ON e.generation_id=a.generation_id AND e.event_id=a.event_id
		WHERE a.generation_id=? ORDER BY e.source_key,e.date_kind,a.role,a.ordinal`, generationID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	claims := []DocumentPeopleActor{}
	for rows.Next() {
		var claim DocumentPeopleActor
		var sensitive int
		if err := rows.Scan(&claim.Role, &claim.ActorKey, &claim.DisplayName, &claim.Address,
			&claim.EvidenceKind, &claim.EvidenceID, &claim.AxisKey, &sensitive); err != nil {
			return nil, err
		}
		claim.Sensitive = sensitive != 0
		claims = append(claims, claim)
	}
	return claims, rows.Err()
}

func peopleBoundToActorKeyTx(ctx context.Context, tx *sql.Tx, actorKey string) ([]Person, error) {
	if strings.HasPrefix(actorKey, "external_uid:") {
		type externalTuple struct{ system, archive, uid, personID string }
		tuples, err := func() ([]externalTuple, error) {
			rows, err := tx.QueryContext(ctx, `SELECT system,archive_id,uid,person_id
				FROM person_external_identities ORDER BY system,archive_id,uid`)
			if err != nil {
				return nil, err
			}
			defer func() { _ = rows.Close() }()
			values := []externalTuple{}
			for rows.Next() {
				var value externalTuple
				if err := rows.Scan(&value.system, &value.archive, &value.uid, &value.personID); err != nil {
					return nil, err
				}
				key, err := document.ExternalPersonActorKey(value.system, value.archive, value.uid)
				if err != nil {
					return nil, err
				}
				if key == actorKey {
					values = append(values, value)
				}
			}
			return values, rows.Err()
		}()
		if err != nil {
			return nil, err
		}
		matches := []Person{}
		for _, value := range tuples {
			resolved, err := resolvePersonUIDTx(ctx, tx, value.system, value.archive, value.uid)
			if errors.Is(err, ErrNotFound) {
				// A retained unlinked tuple is an explicit suppression. Represent it
				// as retired to prevent both publication and address fallback.
				return []Person{{PersonID: value.personID, State: "retired"}}, nil
			}
			if err != nil {
				return nil, err
			}
			var person Person
			err = tx.QueryRowContext(ctx, `SELECT person_id,display_name,display_name_folded,origin,state,revision,created_at,updated_at FROM persons WHERE person_id=?`, resolved.PersonID).
				Scan(&person.PersonID, &person.DisplayName, &person.DisplayNameFolded, &person.Origin,
					&person.State, &person.Revision, &person.CreatedAt, &person.UpdatedAt)
			if err != nil {
				return nil, err
			}
			matches = append(matches, person)
			if len(matches) == 2 {
				break
			}
		}
		return slices.CompactFunc(matches, func(a, b Person) bool { return a.PersonID == b.PersonID }), nil
	}
	kind, value, ok := strings.Cut(actorKey, ":")
	if !ok {
		return []Person{}, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT p.person_id,p.display_name,p.display_name_folded,p.origin,p.state,p.revision,p.created_at,p.updated_at
		FROM person_identities i JOIN persons p ON p.person_id=i.person_id
		WHERE i.kind=? AND i.value_normalized=? ORDER BY p.person_id LIMIT 2`, kind, value)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	matches := []Person{}
	for rows.Next() {
		var p Person
		if err := rows.Scan(&p.PersonID, &p.DisplayName, &p.DisplayNameFolded, &p.Origin, &p.State,
			&p.Revision, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		matches = append(matches, p)
	}
	return matches, rows.Err()
}

func actorKeyAutoProvisionEligible(key string) bool {
	kind, value, ok := strings.Cut(key, ":")
	return ok && (kind == "email" || (kind == "phone" && strings.HasPrefix(value, "+")))
}

func provisionActorPersonTx(ctx context.Context, tx *sql.Tx, claim DocumentPeopleActor) (Person, error) {
	kind, value, _ := strings.Cut(claim.ActorKey, ":")
	display := claim.DisplayLabel()
	if !validPersonName(display) {
		return Person{}, ErrInvalidPerson
	}
	normalized, err := document.NormalizePersonIdentity(document.PersonIdentityKind(kind), value)
	if err != nil || !normalized.AutoLinkEligible {
		return Person{}, ErrInvalidPerson
	}
	personID, err := newUUIDv4()
	if err != nil {
		return Person{}, err
	}
	identityID, err := newUUIDv4()
	if err != nil {
		return Person{}, err
	}
	now := nowRFC3339()
	if _, err := tx.ExecContext(ctx, `INSERT INTO persons(person_id,display_name,display_name_folded,origin,state,revision,created_at,updated_at)
		VALUES(?,?,?,'derived','provisional',1,?,?)`, personID, display, document.FoldPersonName(display), now, now); err != nil {
		return Person{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO person_identities(identity_id,person_id,kind,value_normalized,value_display,scope_kind,scope_value,normalization,origin,evidence_kind,evidence_id,confidence,recorded_at)
		VALUES(?,?,?,?,?,'','',?,'derived',?,?,'exact_identifier',?)`, identityID, personID, kind,
		normalized.ValueNormalized, normalized.ValueDisplay, normalized.Normalization, claim.EvidenceKind, claim.EvidenceID, now); err != nil {
		return Person{}, err
	}
	return Person{PersonID: personID, DisplayName: display, DisplayNameFolded: document.FoldPersonName(display),
		Origin: "derived", State: "provisional", Revision: 1, CreatedAt: now, UpdatedAt: now}, nil
}

func (s *Store) openActorCandidatesTx(ctx context.Context, tx *sql.Tx, versionID string, claims []DocumentPeopleActor, matches []Person) (bool, error) {
	claim := claims[0]
	reason := "identifier_conflict"
	if strings.HasPrefix(claim.ActorKey, "name_alias:") {
		reason = "name_only"
	}
	if strings.HasPrefix(claim.ActorKey, "external_uid:") {
		reason = "external_uid_conflict"
	}
	occurrences := make([]PersonCandidateOccurrence, 0, len(claims))
	for _, occurrence := range claims {
		occurrences = append(occurrences, PersonCandidateOccurrence{ContentVersionID: versionID,
			Role: occurrence.Role, EvidenceKind: occurrence.EvidenceKind, EvidenceID: occurrence.EvidenceID})
	}
	evidence, err := canonical.Marshal(occurrences)
	if err != nil {
		return false, err
	}
	// Keep retired bindings for resolution, but never offer them as suggestions.
	matches = slices.DeleteFunc(slices.Clone(matches), func(match Person) bool { return match.State == "retired" })
	retained := true
	if len(matches) == 0 {
		_, ok, err := s.OpenPersonCandidate(ctx, tx, PersonMatchCandidate{ActorKey: claim.ActorKey,
			DisplayName: claim.DisplayLabel(), Reason: reason, Evidence: evidence})
		return ok, err
	}
	for _, match := range matches {
		_, ok, err := s.OpenPersonCandidate(ctx, tx, PersonMatchCandidate{ActorKey: claim.ActorKey,
			DisplayName: claim.DisplayLabel(), SuggestedPersonID: match.PersonID, Reason: reason, Evidence: evidence})
		if err != nil {
			return false, err
		}
		retained = retained && ok
	}
	return retained, nil
}

func loadDocumentPeopleCustodiansTx(ctx context.Context, tx *sql.Tx, versionID string, input *DocumentPeopleInputs) error {
	rows, err := tx.QueryContext(ctx, `WITH `+CollectionMembershipCTE+`
		SELECT `+custodianColumns+` FROM custodian_assignments ca WHERE ca.retired_at IS NULL AND (
		(ca.scope_kind='document' AND ca.content_version_id=?) OR
		(ca.scope_kind='collection' AND EXISTS(SELECT 1 FROM collection_members cm JOIN nodes n ON n.id=cm.node_id WHERE cm.ingest_id=ca.ingest_id AND n.current_version_id=?)))
		ORDER BY CASE WHEN ca.scope_kind='document' AND ca.basis='operator_assigned' THEN 0 WHEN ca.scope_kind='document' AND ca.basis='transfer_record' THEN 1 ELSE 4 END,ca.recorded_at,ca.assignment_id LIMIT ?`, versionID, versionID, document.MaxPersonEdgesPerVersion+1)
	if err != nil {
		return err
	}
	values, err := scanCustodianRows(rows)
	if err != nil {
		return err
	}
	input.Custodians = values
	return nil
}

func loadDocumentPeopleAssertionsTx(ctx context.Context, tx *sql.Tx, versionID string, input *DocumentPeopleInputs) error {
	rows, err := tx.QueryContext(ctx, `SELECT assertion_id,content_version_id,person_id,role,action,note,recorded_at,revision
		FROM person_document_assertions WHERE content_version_id=? ORDER BY role,person_id,assertion_id LIMIT ?`, versionID, document.MaxPersonEdgesPerVersion+1)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		a, err := scanPersonAssertion(rows)
		if err != nil {
			return err
		}
		input.Assertions = append(input.Assertions, a)
	}
	return rows.Err()
}

func loadReferencedPeopleTx(ctx context.Context, tx *sql.Tx, input *DocumentPeopleInputs) error {
	ids := map[string]bool{}
	for _, matches := range input.Bindings {
		for _, p := range matches {
			ids[p.PersonID] = true
		}
	}
	for _, a := range input.Custodians {
		if a.PersonID != nil {
			ids[*a.PersonID] = true
		}
	}
	for _, a := range input.Assertions {
		ids[a.PersonID] = true
	}
	ordered := make([]string, 0, len(ids))
	for id := range ids {
		ordered = append(ordered, id)
	}
	slices.Sort(ordered)
	for _, id := range ordered {
		var p Person
		err := tx.QueryRowContext(ctx, `SELECT person_id,display_name,display_name_folded,origin,state,revision,created_at,updated_at FROM persons WHERE person_id=?`, id).
			Scan(&p.PersonID, &p.DisplayName, &p.DisplayNameFolded, &p.Origin, &p.State, &p.Revision, &p.CreatedAt, &p.UpdatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		input.Persons[id] = p
	}
	return nil
}

func (s *Store) PublishDocumentPeople(ctx context.Context, p DocumentPeoplePublication) (DocumentPeopleHead, error) {
	if err := validateCatalogSHA256(p.InputsSHA256, "document people inputs digest"); err != nil {
		return DocumentPeopleHead{}, err
	}
	if err := validateCatalogSHA256(p.ResolverFingerprint, "document people resolver fingerprint"); err != nil {
		return DocumentPeopleHead{}, err
	}
	canonicalJSON, checksum, err := document.MarshalDocumentPeopleV1(p.People)
	if err != nil {
		return DocumentPeopleHead{}, fmt.Errorf("validating document people: %w", err)
	}
	generationID, err := documentPeopleGenerationID(p, checksum)
	if err != nil {
		return DocumentPeopleHead{}, err
	}
	state := documentPeopleStatePublished
	if p.Resolution.OverLimit {
		state = documentPeopleStateUnavailable
	}
	var head DocumentPeopleHead
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if err := checkPeoplePublicationFence(ctx, tx, p); err != nil {
			return err
		}
		var storedChecksum string
		var storedJSON []byte
		err := tx.QueryRowContext(ctx, `SELECT checksum,canonical_json FROM document_people_generations WHERE generation_id=?`, generationID).Scan(&storedChecksum, &storedJSON)
		switch {
		case err == nil && (storedChecksum != checksum || !bytes.Equal(storedJSON, canonicalJSON)):
			return ErrDocumentPeopleCorrupt
		case err != nil && !errors.Is(err, sql.ErrNoRows):
			return err
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.ExecContext(ctx, `INSERT INTO document_people_generations(generation_id,content_version_id,inputs_sha256,resolver_fingerprint,canonical_json,checksum,created_at) VALUES(?,?,?,?,?,?,?)`,
				generationID, p.People.ContentVersionID, p.InputsSHA256, p.ResolverFingerprint, canonicalJSON, checksum, nowRFC3339()); err != nil {
				return err
			}
		}
		changed, err := documentPeopleHeadChangedTx(ctx, tx, p, generationID, state)
		if err != nil {
			return err
		}
		if changed {
			if _, err := tx.ExecContext(ctx, `DELETE FROM document_people WHERE content_version_id=?`, p.People.ContentVersionID); err != nil {
				return err
			}
			if state == documentPeopleStatePublished {
				for _, edge := range p.People.Edges {
					if _, err := tx.ExecContext(ctx, `INSERT INTO document_people(content_version_id,node_id,person_id,generation_id,role,actor_key,evidence_kind,evidence_id,confidence,basis,raw_label,claim_count,first_axis_key,last_axis_key,sensitive) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
						p.People.ContentVersionID, p.NodeID, edge.PersonID, generationID, edge.Role, edge.ActorKey,
						edge.EvidenceKind, edge.EvidenceID, edge.Confidence, edge.Basis, edge.RawLabel, edge.ClaimCount,
						nullablePeopleText(edge.FirstAxisKey), nullablePeopleText(edge.LastAxisKey), boolInt(edge.Sensitive)); err != nil {
						return err
					}
				}
			}
			publishedAt := nowRFC3339()
			if _, err := tx.ExecContext(ctx, `UPDATE document_people_state SET publication_epoch=publication_epoch+1,resolver_fingerprint=?,updated_at=? WHERE singleton=1`, p.ResolverFingerprint, publishedAt); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO document_people_heads(content_version_id,event_generation_id,inputs_sha256,generation_id,unresolved_actors,suppressed_actors,candidate_overflow,resolver_fingerprint,binding_epoch,node_revision,edge_count,state,failure_reason,published_at)
				VALUES(?,?,?,?,?,?,?,?,?,?,?,?,'',?) ON CONFLICT(content_version_id) DO UPDATE SET event_generation_id=excluded.event_generation_id,inputs_sha256=excluded.inputs_sha256,generation_id=excluded.generation_id,unresolved_actors=excluded.unresolved_actors,suppressed_actors=excluded.suppressed_actors,candidate_overflow=excluded.candidate_overflow,resolver_fingerprint=excluded.resolver_fingerprint,binding_epoch=excluded.binding_epoch,node_revision=excluded.node_revision,edge_count=excluded.edge_count,state=excluded.state,failure_reason='',published_at=excluded.published_at`,
				p.People.ContentVersionID, p.People.EventGenerationID, p.InputsSHA256, generationID,
				p.Resolution.UnresolvedActors, p.Resolution.SuppressedActors, p.Resolution.CandidateOverflow,
				p.ResolverFingerprint, p.BindingEpoch, p.NodeRevision, len(p.People.Edges), state, publishedAt); err != nil {
				return err
			}
		}
		var scanErr error
		head, scanErr = documentPeopleHeadTx(ctx, tx, p.People.ContentVersionID)
		return scanErr
	})
	return head, err
}

func checkPeoplePublicationFence(ctx context.Context, tx *sql.Tx, p DocumentPeoplePublication) error {
	var epoch, nodeRevision int64
	var eventGeneration string
	if err := tx.QueryRowContext(ctx, `SELECT binding_epoch FROM document_people_state WHERE singleton=1`).Scan(&epoch); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(h.generation_id,''),n.revision FROM content_versions cv JOIN nodes n ON n.id=cv.node_id LEFT JOIN document_event_heads h ON h.content_version_id=cv.version_id WHERE cv.version_id=? AND cv.node_id=?`, p.People.ContentVersionID, p.NodeID).Scan(&eventGeneration, &nodeRevision); err != nil {
		return err
	}
	// Membership, trash, and current-version changes advance the node revision.
	// Other node edits may conservatively rebuild this document, never the vault.
	if epoch != p.BindingEpoch || eventGeneration != p.People.EventGenerationID || nodeRevision != p.NodeRevision {
		return ErrPeopleInputsChanged
	}
	return nil
}

func documentPeopleHeadChangedTx(ctx context.Context, tx *sql.Tx, p DocumentPeoplePublication, generationID, state string) (bool, error) {
	var h DocumentPeopleHead
	err := tx.QueryRowContext(ctx, `SELECT content_version_id,event_generation_id,generation_id,inputs_sha256,resolver_fingerprint,binding_epoch,node_revision,edge_count,state,failure_reason,published_at FROM document_people_heads WHERE content_version_id=?`, p.People.ContentVersionID).
		Scan(&h.ContentVersionID, &h.EventGenerationID, &h.GenerationID, &h.InputsSHA256, &h.ResolverFingerprint, &h.BindingEpoch, &h.NodeRevision, &h.EdgeCount, &h.State, &h.FailureReason, &h.PublishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return h.GenerationID != generationID || h.InputsSHA256 != p.InputsSHA256 || h.ResolverFingerprint != p.ResolverFingerprint || h.BindingEpoch != p.BindingEpoch || h.NodeRevision != p.NodeRevision || h.State != state || h.FailureReason != "" || h.EdgeCount != int64(len(p.People.Edges)), nil
}

func invalidateDocumentPeopleForVersionsTx(ctx context.Context, tx *sql.Tx, versionIDs []string) error {
	var removed int64
	for _, versionID := range slices.Compact(slices.Sorted(slices.Values(versionIDs))) {
		for _, query := range []string{
			`DELETE FROM document_people_heads WHERE content_version_id=?`,
			`DELETE FROM document_people WHERE content_version_id=?`,
		} {
			result, err := tx.ExecContext(ctx, query, versionID)
			if err != nil {
				return err
			}
			count, err := result.RowsAffected()
			if err != nil {
				return err
			}
			removed += count
		}
	}
	if removed == 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE document_people_state
		SET publication_epoch=publication_epoch+1,updated_at=? WHERE singleton=1`, nowRFC3339())
	return err
}

func documentPeopleHeadTx(ctx context.Context, tx *sql.Tx, versionID string) (DocumentPeopleHead, error) {
	var h DocumentPeopleHead
	err := tx.QueryRowContext(ctx, `SELECT content_version_id,event_generation_id,generation_id,inputs_sha256,resolver_fingerprint,binding_epoch,node_revision,edge_count,state,failure_reason,published_at FROM document_people_heads WHERE content_version_id=?`, versionID).
		Scan(&h.ContentVersionID, &h.EventGenerationID, &h.GenerationID, &h.InputsSHA256, &h.ResolverFingerprint, &h.BindingEpoch, &h.NodeRevision, &h.EdgeCount, &h.State, &h.FailureReason, &h.PublishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return h, ErrNotFound
	}
	return h, err
}

func (s *Store) MarkDocumentPeopleFailed(ctx context.Context, input DocumentPeopleInputs, reason string) error {
	if reason != "derivation_failed" && reason != "input_over_limit" {
		return errors.New("invalid document people failure reason")
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		p := DocumentPeoplePublication{People: document.DocumentPeopleV1{ContentVersionID: input.ContentVersionID, EventGenerationID: input.EventGenerationID}, NodeID: input.NodeID, NodeRevision: input.NodeRevision, BindingEpoch: input.BindingEpoch}
		if err := checkPeoplePublicationFence(ctx, tx, p); err != nil {
			return err
		}
		raw, err := canonical.Marshal(input)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		digest := hex.EncodeToString(sum[:])
		var state, eventGeneration, inputsSHA256, resolverFingerprint, failureReason string
		var bindingEpoch int64
		err = tx.QueryRowContext(ctx, `SELECT state,event_generation_id,inputs_sha256,resolver_fingerprint,binding_epoch,failure_reason
			FROM document_people_heads WHERE content_version_id=?`, input.ContentVersionID).
			Scan(&state, &eventGeneration, &inputsSHA256, &resolverFingerprint, &bindingEpoch, &failureReason)
		if err == nil && (state == documentPeopleStatePublished || state == documentPeopleStateUnavailable) &&
			eventGeneration == input.EventGenerationID && inputsSHA256 == digest &&
			resolverFingerprint == document.PersonResolverFingerprint() && bindingEpoch == input.BindingEpoch {
			return nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		headState := documentPeopleStateFailed
		if reason == "input_over_limit" {
			headState = documentPeopleStateUnavailable
		}
		if err == nil && state == headState && eventGeneration == input.EventGenerationID &&
			inputsSHA256 == digest && resolverFingerprint == document.PersonResolverFingerprint() &&
			bindingEpoch == input.BindingEpoch && failureReason == reason {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM document_people WHERE content_version_id=?`, input.ContentVersionID); err != nil {
			return err
		}
		publishedAt := nowRFC3339()
		if _, err := tx.ExecContext(ctx, `UPDATE document_people_state SET publication_epoch=publication_epoch+1,updated_at=? WHERE singleton=1`, publishedAt); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO document_people_heads(content_version_id,event_generation_id,inputs_sha256,generation_id,unresolved_actors,suppressed_actors,candidate_overflow,resolver_fingerprint,binding_epoch,node_revision,edge_count,state,failure_reason,published_at)
			VALUES(?,?,?,'',0,0,0,?,?,?,0,?,?,?) ON CONFLICT(content_version_id) DO UPDATE SET event_generation_id=excluded.event_generation_id,inputs_sha256=excluded.inputs_sha256,generation_id='',unresolved_actors=0,suppressed_actors=0,candidate_overflow=0,resolver_fingerprint=excluded.resolver_fingerprint,binding_epoch=excluded.binding_epoch,node_revision=excluded.node_revision,edge_count=0,state=excluded.state,failure_reason=excluded.failure_reason,published_at=excluded.published_at`,
			input.ContentVersionID, input.EventGenerationID, digest, document.PersonResolverFingerprint(), input.BindingEpoch, input.NodeRevision, headState, reason, publishedAt)
		if err != nil {
			return err
		}
		return nil
	})
}

// DocumentPeopleForVersion returns current attribution and coverage diagnostics.
// Stale publications remain stored, but cannot be read as current attribution.
func (s *Store) DocumentPeopleForVersion(ctx context.Context, versionID string) ([]document.DocumentPersonEdgeV1, DocumentPeopleHead, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, DocumentPeopleHead{}, err
	}
	defer func() { _ = tx.Rollback() }()
	head, err := documentPeopleHeadTx(ctx, tx, versionID)
	if err != nil {
		return nil, head, err
	}
	var current bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM content_versions cv JOIN nodes n ON n.id=cv.node_id
		JOIN document_event_heads eh ON eh.content_version_id=cv.version_id
		CROSS JOIN document_people_state ps
		WHERE cv.version_id=? AND eh.generation_id=? AND ps.binding_epoch=? AND n.revision=?)`,
		versionID, head.EventGenerationID, head.BindingEpoch, head.NodeRevision).Scan(&current)
	if err != nil {
		return nil, head, err
	}
	if !current {
		return nil, head, ErrNotFound
	}
	if head.GenerationID == "" {
		return []document.DocumentPersonEdgeV1{}, head, nil
	}
	var raw []byte
	var checksum string
	if err := tx.QueryRowContext(ctx, `SELECT canonical_json,checksum FROM document_people_generations WHERE generation_id=?`, head.GenerationID).Scan(&raw, &checksum); err != nil {
		return nil, head, ErrDocumentPeopleCorrupt
	}
	record, actual, err := document.DecodeDocumentPeopleV1(raw)
	if err != nil || actual != checksum || record.ContentVersionID != versionID || record.EventGenerationID != head.EventGenerationID || int64(len(record.Edges)) != head.EdgeCount {
		return nil, head, ErrDocumentPeopleCorrupt
	}
	if err := tx.Commit(); err != nil {
		return nil, head, err
	}
	return record.Edges, head, nil
}

func (s *Store) MissingDocumentPeopleTargetsAfter(ctx context.Context, fingerprint, after string, limit int) ([]DocumentPeopleTarget, error) {
	if limit < 1 || limit > 1000 || (after != "" && validateUUIDv4(after) != nil) {
		return nil, ErrInvalidPerson
	}
	rows, err := s.db.QueryContext(ctx, `SELECT cv.version_id,cv.node_id,CASE WHEN ph.content_version_id IS NULL THEN 'missing' ELSE 'stale' END
		FROM content_versions cv JOIN nodes n ON n.id=cv.node_id JOIN document_event_heads eh ON eh.content_version_id=cv.version_id
		LEFT JOIN document_people_heads ph ON ph.content_version_id=cv.version_id
		CROSS JOIN document_people_state ps WHERE cv.version_id>? AND (ph.content_version_id IS NULL OR ph.state='failed' OR ph.event_generation_id<>eh.generation_id OR ph.binding_epoch<>ps.binding_epoch OR ph.node_revision<>n.revision OR ph.resolver_fingerprint<>?)
		ORDER BY cv.version_id LIMIT ?`, after, fingerprint, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	targets := []DocumentPeopleTarget{}
	for rows.Next() {
		var v DocumentPeopleTarget
		if err := rows.Scan(&v.ContentVersionID, &v.NodeID, &v.Reason); err != nil {
			return nil, err
		}
		targets = append(targets, v)
	}
	return targets, rows.Err()
}

func nullablePeopleText(value string) any {
	if value == "" {
		return nil
	}
	return value
}
func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
