package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/query"
)

var ErrInvalidContentMap = document.ErrInvalidContentMap

const (
	mapAvailabilityAvailable   = "available"
	mapAvailabilityUnavailable = "unavailable"
)

// MapAccess is the authority supplied by the authenticated operation boundary.
// An empty permitted set grants no sources. AllSources is reserved for a local
// full-authority API key; scoped callers must supply exact version identities.
type MapAccess struct {
	Owner               string   `json:"-"`
	AllSources          bool     `json:"-"`
	PermittedVersionIDs []string `json:"-"`
}

type ContentMapSection struct {
	ID                 string                   `json:"id"`
	Heading            string                   `json:"heading"`
	Description        string                   `json:"description"`
	HeadingSources     []document.ContentMapPin `json:"heading_sources,omitzero"`
	DescriptionSources []document.ContentMapPin `json:"description_sources,omitzero"`
	Selector           *query.Query             `json:"selector,omitzero"`
	Include            []document.ContentMapPin `json:"include"`
	Exclude            []document.ContentMapPin `json:"exclude"`
	Ordering           string                   `json:"ordering"`
	MaxEntries         int                      `json:"max_entries"`
}

type ContentMapDefinition struct {
	Title           string              `json:"title"`
	Scope           string              `json:"scope"`
	TemplateID      string              `json:"template_id,omitzero"`
	TemplateVersion string              `json:"template_version,omitzero"`
	Sections        []ContentMapSection `json:"sections"`
}

type ContentMap struct {
	ID               string               `json:"id"`
	Owner            string               `json:"owner"`
	Revision         int64                `json:"revision"`
	Definition       ContentMapDefinition `json:"definition"`
	DefinitionDigest string               `json:"definition_digest"`
	CreatedAt        string               `json:"created_at"`
	UpdatedAt        string               `json:"updated_at"`
	ArchivedAt       string               `json:"archived_at,omitzero"`
}

type ContentMapPlan struct {
	Definition       ContentMapDefinition `json:"definition"`
	DefinitionDigest string               `json:"definition_digest"`
	MetadataBytes    int                  `json:"metadata_bytes"`
}

type ContentMapSnapshotEntry struct {
	DocumentUID               string                 `json:"document_uid"`
	Availability              string                 `json:"availability"`
	RequestedContentVersionID string                 `json:"requested_content_version_id,omitzero"`
	Member                    SnapshotMember         `json:"member"`
	Name                      string                 `json:"name"`
	Path                      string                 `json:"path"`
	ModifiedAt                string                 `json:"modified_at"`
	TagCount                  int                    `json:"tag_count"`
	PinMode                   string                 `json:"pin_mode,omitzero"`
	Passage                   *document.PassageRefV1 `json:"passage,omitzero"`
}

type ContentMapSnapshotSection struct {
	ID          string                    `json:"id"`
	Heading     string                    `json:"heading"`
	Description string                    `json:"description"`
	Entries     []ContentMapSnapshotEntry `json:"entries"`
	MemberHash  string                    `json:"member_hash"`
}

type ContentMapSnapshot struct {
	ID               string                      `json:"id"`
	MapID            string                      `json:"map_id"`
	MapRevision      int64                       `json:"map_revision"`
	Owner            string                      `json:"owner"`
	ScopeDigest      string                      `json:"scope_digest"`
	DefinitionDigest string                      `json:"definition_digest"`
	MemberHash       string                      `json:"member_hash"`
	CreatedAt        string                      `json:"created_at"`
	Sections         []ContentMapSnapshotSection `json:"sections"`
}

// Source dependencies stay in the stored snapshot, outside the public result.
// An empty receipt is explicit for newly frozen snapshots.
type contentMapSnapshotStorage struct {
	ContentMapSnapshot

	Dependencies *[]document.ContentMapPin `json:"dependencies,omitzero"`
}

// ValidateContentMapSnapshotRecord checks stored or imported frozen authority
// without consulting current sources. Backup validation can use the same rule.
func ValidateContentMapSnapshotRecord(snapshot ContentMapSnapshot) error {
	encoded, err := json.Marshal(snapshot)
	if err != nil || len(encoded) > document.MaxContentMapMetadataBytes {
		return ErrInvalidContentMap
	}
	if validateUUIDv4(snapshot.ID) != nil || validateUUIDv4(snapshot.MapID) != nil ||
		snapshot.MapRevision < 1 || snapshot.Owner == "" ||
		len(snapshot.Sections) == 0 || len(snapshot.Sections) > document.MaxContentMapSections {
		return ErrInvalidContentMap
	}
	seenSections := make(map[string]bool, len(snapshot.Sections))
	allMembers := make([]SnapshotMember, 0)
	uniqueSources := make(map[string]bool)
	for _, section := range snapshot.Sections {
		if section.ID == "" || seenSections[section.ID] || len(section.Entries) > MaxSearchSourceFenceIDs ||
			len(section.Description) > document.MaxContentMapDescriptionBytes {
			return ErrInvalidContentMap
		}
		seenSections[section.ID] = true
		members := make([]SnapshotMember, 0, len(section.Entries))
		for _, entry := range section.Entries {
			if validateUUIDv4(entry.DocumentUID) != nil {
				return ErrInvalidContentMap
			}
			if entry.PinMode != "" && entry.PinMode != document.MapPinVersionPinned &&
				entry.PinMode != document.MapPinFollowCurrent {
				return ErrInvalidContentMap
			}
			if entry.PinMode == document.MapPinVersionPinned && validateUUIDv4(entry.RequestedContentVersionID) != nil ||
				entry.PinMode != document.MapPinVersionPinned && entry.RequestedContentVersionID != "" {
				return ErrInvalidContentMap
			}
			switch entry.Availability {
			case mapAvailabilityAvailable:
				if entry.Member.NodeID < 1 || validateUUIDv4(entry.Member.ContentVersionID) != nil ||
					len(entry.Member.BlobHash) != 64 || entry.Path == "" {
					return ErrInvalidContentMap
				}
				if entry.PinMode == document.MapPinVersionPinned && entry.Passage == nil &&
					entry.Member.ContentVersionID != entry.RequestedContentVersionID ||
					entry.Passage != nil && (entry.Passage.DocumentUID != entry.DocumentUID ||
						entry.Passage.ContentVersionID != entry.RequestedContentVersionID ||
						document.ValidatePassageIdentityV1(*entry.Passage) != nil) {
					return ErrInvalidContentMap
				}
				members = append(members, entry.Member)
				uniqueSources[entry.Member.ContentVersionID] = true
			case mapAvailabilityUnavailable:
				if entry.PinMode == "" || entry.Member.NodeID != 0 || entry.Member.ContentVersionID != "" {
					return ErrInvalidContentMap
				}
				if entry.Passage != nil && (entry.Passage.DocumentUID != entry.DocumentUID ||
					entry.Passage.ContentVersionID != entry.RequestedContentVersionID ||
					document.ValidatePassageIdentityV1(*entry.Passage) != nil) {
					return ErrInvalidContentMap
				}
			default:
				return ErrInvalidContentMap
			}
		}
		if snapshotMemberHash(members) != section.MemberHash {
			return ErrInvalidContentMap
		}
		allMembers = append(allMembers, members...)
	}
	if len(uniqueSources) > MaxSearchSourceFenceIDs || snapshotMemberHash(allMembers) != snapshot.MemberHash {
		return ErrInvalidContentMap
	}
	return nil
}

func validateMapAccess(access MapAccess) error {
	if access.Owner == "" || len(access.Owner) > 256 || !utf8.ValidString(access.Owner) || strings.ContainsRune(access.Owner, 0) {
		return ErrInvalidContentMap
	}
	if len(access.PermittedVersionIDs) > MaxSearchSourceFenceIDs {
		return &ProcessingSourceFenceScopeError{ObservedScopeCount: len(access.PermittedVersionIDs)}
	}
	for _, id := range access.PermittedVersionIDs {
		if validateUUIDv4(id) != nil {
			return ErrInvalidContentMap
		}
	}
	return nil
}

func mapPermits(access MapAccess, versionID string) bool {
	return access.AllSources || slices.Contains(access.PermittedVersionIDs, versionID)
}

func mapScopeDigest(access MapAccess) string {
	ids := slices.Clone(access.PermittedVersionIDs)
	slices.Sort(ids)
	if access.AllSources {
		ids = []string{"*"}
	}
	sum := sha256.Sum256([]byte(access.Owner + "\x00" + strings.Join(ids, "\x00")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func normalizedMapDefinition(definition ContentMapDefinition) (ContentMapPlan, []byte, error) {
	if definition.Title == "" || len(definition.Title) > 256 || !utf8.ValidString(definition.Title) ||
		definition.Scope == "" || len(definition.Scope) > 256 || !utf8.ValidString(definition.Scope) ||
		len(definition.Sections) == 0 || len(definition.Sections) > document.MaxContentMapSections {
		return ContentMapPlan{}, nil, ErrInvalidContentMap
	}
	if len(definition.TemplateID) > 256 || len(definition.TemplateVersion) > 64 ||
		!utf8.ValidString(definition.TemplateID) || !utf8.ValidString(definition.TemplateVersion) {
		return ContentMapPlan{}, nil, ErrInvalidContentMap
	}
	seenSections := make(map[string]bool, len(definition.Sections))
	allPins := 0
	for index := range definition.Sections {
		section := &definition.Sections[index]
		if section.ID == "" || len(section.ID) > 64 || strings.ContainsAny(section.ID, "/\\\x00") ||
			!utf8.ValidString(section.ID) || seenSections[section.ID] ||
			section.Heading == "" || len(section.Heading) > 256 || !utf8.ValidString(section.Heading) ||
			len(section.Description) > document.MaxContentMapDescriptionBytes || !utf8.ValidString(section.Description) ||
			section.MaxEntries < 1 || section.MaxEntries > MaxSearchSourceFenceIDs || section.MaxEntries < len(section.Include) {
			return ContentMapPlan{}, nil, ErrInvalidContentMap
		}
		seenSections[section.ID] = true
		if section.Ordering != "explicit" && section.Ordering != "chronological" && section.Ordering != "relevance" && section.Ordering != "topic_frequency" {
			return ContentMapPlan{}, nil, ErrInvalidContentMap
		}
		if section.Selector != nil {
			canonical, err := query.Canonical(*section.Selector)
			if err != nil {
				return ContentMapPlan{}, nil, fmt.Errorf("%w: selector: %w", ErrInvalidContentMap, err)
			}
			normalized, err := query.Parse(canonical)
			if err != nil {
				return ContentMapPlan{}, nil, err
			}
			section.Selector = &normalized
		}
		allPins += len(section.Include) + len(section.Exclude) + len(section.HeadingSources) + len(section.DescriptionSources)
		if allPins > document.MaxContentMapPins {
			return ContentMapPlan{}, nil, ErrInvalidContentMap
		}
		include, exclude := make([]string, 0, len(section.Include)), make([]string, 0, len(section.Exclude))
		seenPins := make(map[string]bool)
		for _, pin := range section.Include {
			identity, err := document.ContentMapPinIdentity(pin)
			if err != nil || seenPins[identity] {
				return ContentMapPlan{}, nil, ErrInvalidContentMap
			}
			seenPins[identity] = true
			include = append(include, pin.DocumentUID)
		}
		seenPins = make(map[string]bool)
		for _, pin := range section.Exclude {
			identity, err := document.ContentMapPinIdentity(pin)
			if err != nil || seenPins[identity] {
				return ContentMapPlan{}, nil, ErrInvalidContentMap
			}
			seenPins[identity] = true
			exclude = append(exclude, pin.DocumentUID)
		}
		for _, pin := range append(slices.Clone(section.HeadingSources), section.DescriptionSources...) {
			if _, err := document.ContentMapPinIdentity(pin); err != nil {
				return ContentMapPlan{}, nil, ErrInvalidContentMap
			}
		}
		if err := document.ValidateMapPins(include, exclude); err != nil {
			return ContentMapPlan{}, nil, err
		}
		if section.Selector == nil && len(section.Include) == 0 {
			return ContentMapPlan{}, nil, ErrInvalidContentMap
		}
	}
	encoded, err := json.Marshal(definition)
	if err != nil || len(encoded) > document.MaxContentMapMetadataBytes {
		return ContentMapPlan{}, nil, ErrInvalidContentMap
	}
	sum := sha256.Sum256(encoded)
	plan := ContentMapPlan{Definition: definition, DefinitionDigest: "sha256:" + hex.EncodeToString(sum[:]), MetadataBytes: len(encoded)}
	return plan, encoded, nil
}

// PreviewContentMap validates the complete structured definition without writing it.
func (s *Store) PreviewContentMap(ctx context.Context, access MapAccess, definition ContentMapDefinition) (ContentMapPlan, error) {
	if err := validateMapAccess(access); err != nil {
		return ContentMapPlan{}, err
	}
	plan, _, err := normalizedMapDefinition(definition)
	if err != nil {
		return ContentMapPlan{}, err
	}
	if err := s.validateMapSources(ctx, access, plan.Definition); err != nil {
		return ContentMapPlan{}, err
	}
	return plan, err
}

// CreateContentMap accepts exactly the previewed definition digest.
func (s *Store) CreateContentMap(ctx context.Context, access MapAccess, definition ContentMapDefinition, acceptedDigest string) (ContentMap, error) {
	if err := validateMapAccess(access); err != nil {
		return ContentMap{}, err
	}
	plan, encoded, err := normalizedMapDefinition(definition)
	if err != nil {
		return ContentMap{}, err
	}
	if acceptedDigest != plan.DefinitionDigest {
		return ContentMap{}, ErrStaleRevision
	}
	if err := s.validateMapSources(ctx, access, plan.Definition); err != nil {
		return ContentMap{}, err
	}
	id, err := newUUIDv4()
	if err != nil {
		return ContentMap{}, err
	}
	now := nowRFC3339()
	created := ContentMap{ID: id, Owner: access.Owner, Revision: 1, Definition: plan.Definition,
		DefinitionDigest: plan.DefinitionDigest, CreatedAt: now, UpdatedAt: now}
	err = s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		if err := s.validateMapSourcesTx(ctx, tx, access, plan.Definition); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO content_maps(id,owner,revision,definition_json,definition_digest,created_at,updated_at,archived_at)
			VALUES(?,?,?,?,?,?,?,NULL)`, id, access.Owner, 1, encoded, plan.DefinitionDigest, now, now)
		return err
	})
	return created, err
}

func (s *Store) ContentMapByID(ctx context.Context, access MapAccess, id string) (ContentMap, error) {
	if err := validateMapAccess(access); err != nil {
		return ContentMap{}, err
	}
	if validateUUIDv4(id) != nil {
		return ContentMap{}, ErrNotFound
	}
	result, err := scanContentMap(s.db.QueryRowContext(ctx, `SELECT id,owner,revision,definition_json,definition_digest,created_at,updated_at,COALESCE(archived_at,'')
		FROM content_maps WHERE id=? AND owner=?`, id, access.Owner))
	if err != nil {
		return ContentMap{}, err
	}
	if err := s.authorizeMapDefinition(ctx, access, result.Definition); err != nil {
		return ContentMap{}, err
	}
	return result, nil
}

func scanContentMap(row *sql.Row) (ContentMap, error) {
	var result ContentMap
	var encoded []byte
	err := row.Scan(&result.ID, &result.Owner, &result.Revision, &encoded, &result.DefinitionDigest,
		&result.CreatedAt, &result.UpdatedAt, &result.ArchivedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ContentMap{}, ErrNotFound
	}
	if err != nil {
		return ContentMap{}, err
	}
	if len(encoded) > document.MaxContentMapMetadataBytes || json.Unmarshal(encoded, &result.Definition) != nil {
		return ContentMap{}, ErrInvalidContentMap
	}
	plan, _, err := normalizedMapDefinition(result.Definition)
	if err != nil || plan.DefinitionDigest != result.DefinitionDigest {
		return ContentMap{}, ErrInvalidContentMap
	}
	return result, nil
}

// UpdateContentMap compares the prior revision in one transaction. The whole
// definition is replaced, retaining stable section IDs supplied by the caller.
func (s *Store) UpdateContentMap(ctx context.Context, access MapAccess, id string, expectedRevision int64,
	definition ContentMapDefinition, acceptedDigest string) (ContentMap, error) {
	if err := validateMapAccess(access); err != nil {
		return ContentMap{}, err
	}
	if validateUUIDv4(id) != nil {
		return ContentMap{}, ErrNotFound
	}
	plan, encoded, err := normalizedMapDefinition(definition)
	if err != nil {
		return ContentMap{}, err
	}
	if acceptedDigest != plan.DefinitionDigest {
		return ContentMap{}, ErrStaleRevision
	}
	if err := s.validateMapSources(ctx, access, plan.Definition); err != nil {
		return ContentMap{}, err
	}
	var updated ContentMap
	err = s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		current, err := scanContentMap(tx.QueryRowContext(ctx, `SELECT id,owner,revision,definition_json,definition_digest,created_at,updated_at,COALESCE(archived_at,'')
			FROM content_maps WHERE id=? AND owner=?`, id, access.Owner))
		if err != nil {
			return err
		}
		if current.ArchivedAt != "" {
			return ErrNotFound
		}
		if current.Revision != expectedRevision {
			return ErrStaleRevision
		}
		if err := s.validateMapSourcesTx(ctx, tx, access, plan.Definition); err != nil {
			return err
		}
		if current.DefinitionDigest == plan.DefinitionDigest {
			updated = current
			return nil
		}
		updated = current
		updated.Revision++
		updated.Definition = plan.Definition
		updated.DefinitionDigest = plan.DefinitionDigest
		updated.UpdatedAt = nowRFC3339()
		result, err := tx.ExecContext(ctx, `UPDATE content_maps SET revision=?,definition_json=?,definition_digest=?,updated_at=?
			WHERE id=? AND owner=? AND revision=? AND archived_at IS NULL`, updated.Revision, encoded,
			updated.DefinitionDigest, updated.UpdatedAt, id, access.Owner, expectedRevision)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows != 1 {
			return ErrStaleRevision
		}
		return nil
	})
	return updated, err
}

func (s *Store) validateMapSources(ctx context.Context, access MapAccess, definition ContentMapDefinition) error {
	for _, section := range definition.Sections {
		if section.Selector != nil && !access.AllSources {
			// QueryV1 does not accept an exact source fence. Until the shared query
			// service can push a principal fence into SQL, fail closed.
			return ErrNotFound
		}
		if section.Selector != nil {
			if _, err := s.CompileQuery(ctx, *section.Selector); err != nil {
				return fmt.Errorf("%w: selector: %w", ErrInvalidContentMap, err)
			}
		}
		if err := validateMapPinConflicts(section, func(pin document.ContentMapPin) (ContentMapSnapshotEntry, error) {
			return s.resolveMapPin(ctx, access, pin)
		}); err != nil {
			return err
		}
		for _, pin := range append(slices.Clone(section.HeadingSources), section.DescriptionSources...) {
			if _, err := s.resolveMapPin(ctx, access, pin); err != nil {
				return ErrNotFound
			}
		}
	}
	return nil
}

// validateMapSourcesTx closes the interval between preview/preflight and the
// durable definition write. All source reads use the same SQLite transaction.
func (s *Store) validateMapSourcesTx(ctx context.Context, tx *sql.Tx, access MapAccess, definition ContentMapDefinition) error {
	for _, section := range definition.Sections {
		if section.Selector != nil {
			if !access.AllSources {
				return ErrNotFound
			}
			if _, err := compileQuery(ctx, *section.Selector, queryResolver{q: tx}); err != nil {
				return fmt.Errorf("%w: selector: %w", ErrInvalidContentMap, err)
			}
		}
		if err := validateMapPinConflicts(section, func(pin document.ContentMapPin) (ContentMapSnapshotEntry, error) {
			return s.resolveMapPinTx(ctx, tx, access, pin)
		}); err != nil {
			return err
		}
		for _, pin := range append(slices.Clone(section.HeadingSources), section.DescriptionSources...) {
			if _, err := s.resolveMapPinTx(ctx, tx, access, pin); err != nil {
				return ErrNotFound
			}
		}
	}
	return nil
}

func validateMapPinConflicts(section ContentMapSection,
	resolve func(document.ContentMapPin) (ContentMapSnapshotEntry, error)) error {
	included := make(map[int64]bool, len(section.Include))
	for _, pin := range section.Include {
		entry, err := resolve(pin)
		if err != nil || entry.Member.NodeID < 1 {
			return ErrNotFound
		}
		included[entry.Member.NodeID] = true
	}
	for _, pin := range section.Exclude {
		entry, err := resolve(pin)
		if err != nil || entry.Member.NodeID < 1 {
			return ErrNotFound
		}
		if included[entry.Member.NodeID] {
			return ErrInvalidContentMap
		}
	}
	return nil
}

// resolveMapPinTx performs the same source check as preview against the write
// transaction, including adopted and retained rendition authority.
func (s *Store) resolveMapPinTx(ctx context.Context, tx *sql.Tx, access MapAccess, pin document.ContentMapPin) (ContentMapSnapshotEntry, error) {
	if _, err := document.ContentMapPinIdentity(pin); err != nil {
		return ContentMapSnapshotEntry{}, ErrNotFound
	}
	var nodeID int64
	versionID, buildID, attachmentID := pin.ContentVersionID, "", ""
	if pin.Passage != nil {
		ref := *pin.Passage
		if document.ValidatePassageAddressV1(ref) != nil {
			return ContentMapSnapshotEntry{}, ErrNotFound
		}
		target := adoptedPassageTuple{DocumentUID: ref.DocumentUID, VersionID: ref.ContentVersionID,
			BuildID: ref.RenditionBuildID, AttachmentID: ref.AttachmentID}
		if ref.FederationDomainUID != "" || ref.VaultUID != s.vaultID {
			var err error
			target, err = adoptedPassageAuthorityTx(ctx, tx, ref)
			if err != nil {
				return ContentMapSnapshotEntry{}, ErrNotFound
			}
		}
		versionID, buildID, attachmentID = target.VersionID, target.BuildID, target.AttachmentID
		if err := tx.QueryRowContext(ctx, `SELECT node_id FROM document_identities WHERE document_uid=?`, target.DocumentUID).Scan(&nodeID); err != nil {
			return ContentMapSnapshotEntry{}, ErrNotFound
		}
	} else if err := tx.QueryRowContext(ctx, `SELECT node_id FROM document_identities WHERE document_uid=?`, pin.DocumentUID).Scan(&nodeID); err != nil {
		return ContentMapSnapshotEntry{}, ErrNotFound
	}
	node, err := nodeByIDTx(tx, nodeID)
	if err != nil || node.IsDir() || node.TrashedAt != nil {
		return ContentMapSnapshotEntry{}, ErrNotFound
	}
	if pin.Mode == document.MapPinFollowCurrent {
		versionID = node.CurrentVersionID
	}
	if !mapPermits(access, versionID) {
		return ContentMapSnapshotEntry{}, ErrNotFound
	}
	version, err := scanContentVersion(tx.QueryRowContext(ctx, `SELECT `+contentVersionCols+` FROM content_versions WHERE version_id=? AND node_id=?`, versionID, nodeID))
	if err != nil {
		return ContentMapSnapshotEntry{}, ErrNotFound
	}
	if pin.Passage != nil {
		ref := *pin.Passage
		if version.BlobHash != ref.SourceSHA256 {
			return ContentMapSnapshotEntry{}, ErrNotFound
		}
		var verified bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM rendition_attachments a
			JOIN rendition_builds b ON b.build_id=a.build_id AND b.vault_uid=a.vault_uid
			JOIN rendition_artifacts artifact ON artifact.build_id=b.build_id
			WHERE a.attachment_id=? AND a.vault_uid=? AND a.content_version_id=?
			AND a.build_id=? AND b.source_sha256=?
			AND artifact.role=? AND artifact.blob_hash=artifact.checksum
			AND artifact.blob_hash=b.markdown_checksum
		)`, attachmentID, s.vaultID, versionID, buildID, ref.SourceSHA256,
			catalogArtifactSanitizedMarkdown).Scan(&verified)
		if err != nil {
			return ContentMapSnapshotEntry{}, ErrNotFound
		}
		if !verified {
			return ContentMapSnapshotEntry{}, ErrNotFound
		}
	}
	path, err := pathOf(ctx, tx, nodeID)
	if err != nil {
		return ContentMapSnapshotEntry{}, ErrNotFound
	}
	return ContentMapSnapshotEntry{DocumentUID: pin.DocumentUID, Availability: mapAvailabilityAvailable,
		RequestedContentVersionID: pin.ContentVersionID,
		Member: SnapshotMember{NodeID: nodeID, ContentVersionID: version.ID, BlobHash: version.BlobHash,
			Size: version.Size, Revision: version.NodeRevision},
		Name: node.Name, Path: path, ModifiedAt: node.ModifiedAt, PinMode: pin.Mode, Passage: pin.Passage}, nil
}

func (s *Store) authorizeMapDefinition(ctx context.Context, access MapAccess, definition ContentMapDefinition) error {
	for _, section := range definition.Sections {
		if section.Selector != nil && !access.AllSources {
			return ErrNotFound
		}
		for _, pin := range mapSectionPins(section) {
			if access.AllSources && pin.Passage == nil {
				continue
			}
			if pin.Passage != nil {
				if _, err := s.resolveMapPin(ctx, access, pin); err != nil {
					return ErrNotFound
				}
				continue
			}
			versionID := pin.ContentVersionID
			if pin.Mode == document.MapPinFollowCurrent {
				var nodeID int64
				if err := s.db.QueryRowContext(ctx, `SELECT node_id FROM document_identities WHERE document_uid=?`,
					pin.DocumentUID).Scan(&nodeID); err != nil {
					return ErrNotFound
				}
				node, err := s.NodeByID(ctx, nodeID)
				if err != nil || node.TrashedAt != nil {
					return ErrNotFound
				}
				versionID = node.CurrentVersionID
			}
			if !mapPermits(access, versionID) {
				return ErrNotFound
			}
		}
	}
	return nil
}

func mapSectionPins(section ContentMapSection) []document.ContentMapPin {
	pins := make([]document.ContentMapPin, 0, len(section.Include)+len(section.Exclude)+len(section.HeadingSources)+len(section.DescriptionSources))
	pins = append(pins, section.Include...)
	pins = append(pins, section.Exclude...)
	pins = append(pins, section.HeadingSources...)
	return append(pins, section.DescriptionSources...)
}

func mapHiddenDependencies(definition ContentMapDefinition) []document.ContentMapPin {
	dependencies := make([]document.ContentMapPin, 0)
	for _, section := range definition.Sections {
		dependencies = append(dependencies, section.Exclude...)
		dependencies = append(dependencies, section.HeadingSources...)
		dependencies = append(dependencies, section.DescriptionSources...)
	}
	return dependencies
}

func (s *Store) resolveMapPin(ctx context.Context, access MapAccess, pin document.ContentMapPin) (ContentMapSnapshotEntry, error) {
	if _, err := document.ContentMapPinIdentity(pin); err != nil {
		return ContentMapSnapshotEntry{}, ErrNotFound
	}
	if pin.Passage != nil {
		authority, err := s.ResolvePassageAuthority(ctx, *pin.Passage)
		if err != nil || !mapPermits(access, authority.Version.ID) {
			return ContentMapSnapshotEntry{}, ErrNotFound
		}
		return ContentMapSnapshotEntry{DocumentUID: pin.DocumentUID, Availability: mapAvailabilityAvailable,
			RequestedContentVersionID: pin.ContentVersionID,
			Member: SnapshotMember{NodeID: authority.Node.ID, ContentVersionID: authority.Version.ID,
				BlobHash: authority.Version.BlobHash, Size: authority.Version.Size,
				Revision: authority.Version.NodeRevision},
			Name: authority.Node.Name, Path: authority.Path, ModifiedAt: authority.Node.ModifiedAt,
			PinMode: pin.Mode, Passage: pin.Passage}, nil
	}
	var nodeID int64
	err := s.db.QueryRowContext(ctx, `SELECT node_id FROM document_identities WHERE document_uid=?`, pin.DocumentUID).Scan(&nodeID)
	if err != nil {
		return ContentMapSnapshotEntry{}, ErrNotFound
	}
	node, err := s.NodeByID(ctx, nodeID)
	if err != nil || node.IsDir() || node.TrashedAt != nil {
		return ContentMapSnapshotEntry{}, ErrNotFound
	}
	versionID := pin.ContentVersionID
	if pin.Mode == document.MapPinFollowCurrent {
		versionID = node.CurrentVersionID
	}
	if !mapPermits(access, versionID) {
		return ContentMapSnapshotEntry{}, ErrNotFound
	}
	version, err := s.ContentVersionByID(ctx, versionID)
	if err != nil || version.NodeID != nodeID {
		return ContentMapSnapshotEntry{}, ErrNotFound
	}
	path, err := s.Path(ctx, nodeID)
	if err != nil {
		return ContentMapSnapshotEntry{}, ErrNotFound
	}
	return ContentMapSnapshotEntry{DocumentUID: pin.DocumentUID, Availability: mapAvailabilityAvailable,
		RequestedContentVersionID: pin.ContentVersionID,
		Member: SnapshotMember{NodeID: nodeID, ContentVersionID: version.ID, BlobHash: version.BlobHash,
			Size: version.Size, Revision: version.NodeRevision},
		Name: node.Name, Path: path, ModifiedAt: node.ModifiedAt, PinMode: pin.Mode, Passage: pin.Passage}, nil
}

// mapPassageIdentityTx checks the local identity behind a passage citation.
// Unlike rendition resolution, it remains valid when retained bytes disappear.
func (s *Store) mapPassageIdentityTx(ctx context.Context, tx *sql.Tx, entry ContentMapSnapshotEntry) (string, error) {
	ref := entry.Passage
	if ref == nil || document.ValidatePassageIdentityV1(*ref) != nil ||
		ref.DocumentUID != entry.DocumentUID || ref.ContentVersionID != entry.RequestedContentVersionID {
		return "", ErrNotFound
	}
	target := adoptedPassageTuple{DocumentUID: ref.DocumentUID, VersionID: ref.ContentVersionID}
	if ref.FederationDomainUID != "" || ref.VaultUID != s.vaultID {
		var err error
		target, err = adoptedPassageAuthorityTx(ctx, tx, *ref)
		if err != nil {
			return "", ErrNotFound
		}
	}
	var nodeID int64
	if err := tx.QueryRowContext(ctx, `SELECT node_id FROM document_identities WHERE document_uid=?`,
		target.DocumentUID).Scan(&nodeID); err != nil {
		return "", ErrNotFound
	}
	if entry.Availability == mapAvailabilityAvailable &&
		(entry.Member.NodeID != nodeID || entry.Member.ContentVersionID != target.VersionID) {
		return "", ErrNotFound
	}
	return target.VersionID, nil
}

func (s *Store) mapPassageIdentity(ctx context.Context, entry ContentMapSnapshotEntry) (string, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	return s.mapPassageIdentityTx(ctx, tx, entry)
}

// CreateContentMapSnapshot materializes immutable exact-version membership.
// A revision fence prevents an editor from freezing a definition they did not
// inspect. Selectors reuse QuerySnapshot's bounded rows and member receipts.
func (s *Store) CreateContentMapSnapshot(ctx context.Context, access MapAccess, id string, expectedRevision int64) (ContentMapSnapshot, error) {
	return s.createContentMapSnapshot(ctx, access, id, expectedRevision, "")
}

// ContentMapRefresh returns a new immutable snapshot and a bounded comparison
// against the caller's last observed snapshot.
type ContentMapRefresh struct {
	Snapshot ContentMapSnapshot `json:"snapshot"`
	Delta    ContentMapDelta    `json:"delta"`
}

func (s *Store) RefreshContentMap(ctx context.Context, access MapAccess, id string,
	expectedRevision int64, previousSnapshotID string,
) (ContentMapRefresh, error) {
	previous, err := s.ContentMapSnapshotByID(ctx, access, previousSnapshotID)
	if err != nil {
		return ContentMapRefresh{}, err
	}
	if previous.MapID != id {
		return ContentMapRefresh{}, ErrNotFound
	}
	next, err := s.createContentMapSnapshot(ctx, access, id, expectedRevision, previousSnapshotID)
	if err != nil {
		return ContentMapRefresh{}, err
	}
	return ContentMapRefresh{Snapshot: next, Delta: diffContentMapSnapshots(previous, next)}, nil
}

func (s *Store) createContentMapSnapshot(ctx context.Context, access MapAccess, id string,
	expectedRevision int64, previousSnapshotID string,
) (ContentMapSnapshot, error) {
	mapRecord, err := s.ContentMapByID(ctx, access, id)
	if err != nil {
		return ContentMapSnapshot{}, err
	}
	if mapRecord.ArchivedAt != "" {
		return ContentMapSnapshot{}, ErrNotFound
	}
	if mapRecord.Revision != expectedRevision {
		return ContentMapSnapshot{}, ErrStaleRevision
	}
	if err := s.authorizeMapDefinition(ctx, access, mapRecord.Definition); err != nil {
		return ContentMapSnapshot{}, err
	}
	snapshotID, err := newUUIDv4()
	if err != nil {
		return ContentMapSnapshot{}, err
	}
	snapshot := ContentMapSnapshot{ID: snapshotID, MapID: id, MapRevision: mapRecord.Revision,
		Owner: access.Owner, ScopeDigest: mapScopeDigest(access), DefinitionDigest: mapRecord.DefinitionDigest,
		CreatedAt: nowRFC3339(), Sections: make([]ContentMapSnapshotSection, 0, len(mapRecord.Definition.Sections))}
	allMembers := make([]SnapshotMember, 0)
	uniqueSources := make(map[string]bool)
	for _, section := range mapRecord.Definition.Sections {
		resolved := ContentMapSnapshotSection{ID: section.ID, Heading: section.Heading,
			Description: section.Description, Entries: make([]ContentMapSnapshotEntry, 0)}
		excluded := make(map[string]bool, len(section.Exclude))
		for _, pin := range section.Exclude {
			excluded[pin.DocumentUID] = true
			if pin.Passage != nil {
				entry, err := s.resolveMapPin(ctx, access, pin)
				if err != nil {
					return ContentMapSnapshot{}, ErrNotFound
				}
				var localUID string
				if err := s.db.QueryRowContext(ctx, `SELECT document_uid FROM document_identities WHERE node_id=?`,
					entry.Member.NodeID).Scan(&localUID); err != nil {
					return ContentMapSnapshot{}, ErrNotFound
				}
				excluded[localUID] = true
			}
		}
		seen := make(map[string]bool)
		for _, pin := range section.Include {
			entry, err := s.resolveMapPin(ctx, access, pin)
			if err != nil {
				if pin.Passage != nil {
					identityEntry := ContentMapSnapshotEntry{DocumentUID: pin.DocumentUID,
						RequestedContentVersionID: pin.ContentVersionID, Passage: pin.Passage}
					versionID, identityErr := s.mapPassageIdentity(ctx, identityEntry)
					if identityErr != nil || !mapPermits(access, versionID) {
						return ContentMapSnapshot{}, ErrNotFound
					}
				}
				if !access.AllSources && (pin.Mode != document.MapPinVersionPinned ||
					!mapPermits(access, pin.ContentVersionID)) {
					return ContentMapSnapshot{}, ErrNotFound
				}
				entry = ContentMapSnapshotEntry{DocumentUID: pin.DocumentUID,
					Availability: mapAvailabilityUnavailable, RequestedContentVersionID: pin.ContentVersionID,
					PinMode: pin.Mode, Passage: pin.Passage}
			}
			if entry.Availability == mapAvailabilityAvailable {
				if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM node_tags WHERE node_id=?`,
					entry.Member.NodeID).Scan(&entry.TagCount); err != nil {
					return ContentMapSnapshot{}, fmt.Errorf("counting map pin tags: %w", err)
				}
			}
			if entry.Availability == mapAvailabilityAvailable && pin.Passage != nil {
				var localUID string
				if err := s.db.QueryRowContext(ctx, `SELECT document_uid FROM document_identities WHERE node_id=?`,
					entry.Member.NodeID).Scan(&localUID); err != nil {
					return ContentMapSnapshot{}, ErrNotFound
				}
				if excluded[localUID] {
					return ContentMapSnapshot{}, ErrInvalidContentMap
				}
			}
			if excluded[pin.DocumentUID] {
				return ContentMapSnapshot{}, ErrInvalidContentMap
			}
			seen[entry.DocumentUID] = true
			resolved.Entries = append(resolved.Entries, entry)
		}
		if section.Selector != nil {
			projection, err := s.materializeQuerySnapshot(ctx, SnapshotRequest{Query: *section.Selector},
				snapshotMaterializeOptions{MaxRows: MaxSearchSourceFenceIDs})
			if err != nil {
				return ContentMapSnapshot{}, err
			}
			for _, row := range projection.Rows {
				if !mapPermits(access, row.ContentVersionID) {
					continue
				}
				identity, err := s.EnsureDocumentIdentity(ctx, row.NodeID)
				if err != nil {
					return ContentMapSnapshot{}, err
				}
				if excluded[identity.DocumentUID] || seen[identity.DocumentUID] {
					continue
				}
				seen[identity.DocumentUID] = true
				resolved.Entries = append(resolved.Entries, ContentMapSnapshotEntry{
					DocumentUID: identity.DocumentUID, Availability: mapAvailabilityAvailable, Member: row.SnapshotMember,
					Name: row.Name, Path: row.Path, ModifiedAt: row.ModifiedAt, TagCount: len(row.Tags),
				})
			}
		}
		switch section.Ordering {
		case "chronological":
			slices.SortStableFunc(resolved.Entries, func(a, b ContentMapSnapshotEntry) int {
				return strings.Compare(b.ModifiedAt, a.ModifiedAt)
			})
		case "topic_frequency":
			slices.SortStableFunc(resolved.Entries, func(a, b ContentMapSnapshotEntry) int {
				return b.TagCount - a.TagCount
			})
		}
		if len(resolved.Entries) > section.MaxEntries {
			selected := make(map[string]bool)
			remaining := section.MaxEntries - len(section.Include)
			for _, entry := range resolved.Entries {
				if entry.PinMode != "" {
					selected[entry.DocumentUID] = true
				}
			}
			for _, entry := range resolved.Entries {
				if entry.PinMode == "" && remaining > 0 {
					selected[entry.DocumentUID] = true
					remaining--
				}
			}
			kept := make([]ContentMapSnapshotEntry, 0, section.MaxEntries)
			for _, entry := range resolved.Entries {
				if selected[entry.DocumentUID] {
					kept = append(kept, entry)
				}
			}
			resolved.Entries = kept
		}
		sectionMembers := make([]SnapshotMember, 0, len(resolved.Entries))
		for _, entry := range resolved.Entries {
			if entry.Availability == mapAvailabilityAvailable {
				sectionMembers = append(sectionMembers, entry.Member)
			}
		}
		resolved.MemberHash = snapshotMemberHash(sectionMembers)
		allMembers = append(allMembers, sectionMembers...)
		for _, member := range sectionMembers {
			uniqueSources[member.ContentVersionID] = true
		}
		if len(uniqueSources) > MaxSearchSourceFenceIDs {
			return ContentMapSnapshot{}, &ProcessingSourceFenceScopeError{ObservedScopeCount: len(uniqueSources)}
		}
		snapshot.Sections = append(snapshot.Sections, resolved)
	}
	snapshot.MemberHash = snapshotMemberHash(allMembers)
	dependencies := mapHiddenDependencies(mapRecord.Definition)
	if err := ValidateContentMapSnapshotRecord(snapshot); err != nil {
		return ContentMapSnapshot{}, err
	}
	encoded, err := json.Marshal(contentMapSnapshotStorage{ContentMapSnapshot: snapshot, Dependencies: &dependencies})
	if err != nil {
		return ContentMapSnapshot{}, err
	}
	if len(encoded) > document.MaxContentMapMetadataBytes {
		return ContentMapSnapshot{}, ErrQuerySnapshotTooLarge
	}
	err = s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		var currentRevision int64
		if err := tx.QueryRowContext(ctx, `SELECT revision FROM content_maps WHERE id=? AND owner=? AND archived_at IS NULL`,
			id, access.Owner).Scan(&currentRevision); err != nil {
			return ErrNotFound
		}
		if currentRevision != expectedRevision {
			return ErrStaleRevision
		}
		if previousSnapshotID != "" {
			var latestID string
			if err := tx.QueryRowContext(ctx, `SELECT id FROM content_map_snapshots WHERE map_id=? AND owner=?
				ORDER BY rowid DESC LIMIT 1`, id, access.Owner).Scan(&latestID); err != nil {
				return ErrStaleRevision
			}
			if latestID != previousSnapshotID {
				return ErrStaleRevision
			}
		}
		if err := s.authorizeMapSnapshotTx(ctx, tx, access, mapRecord.Definition, snapshot); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO content_map_snapshots(id,map_id,map_revision,owner,scope_digest,definition_digest,snapshot_json,member_hash,created_at)
			VALUES(?,?,?,?,?,?,?,?,?)`, snapshot.ID, id, expectedRevision, access.Owner, snapshot.ScopeDigest,
			snapshot.DefinitionDigest, encoded, snapshot.MemberHash, snapshot.CreatedAt)
		return err
	})
	return snapshot, err
}

// authorizeMapSnapshotTx checks every dependency after membership resolution
// and before publication, against the same transaction as the insert.
func (s *Store) authorizeMapSnapshotTx(ctx context.Context, tx *sql.Tx, access MapAccess,
	definition ContentMapDefinition, snapshot ContentMapSnapshot) error {
	if len(definition.Sections) != len(snapshot.Sections) {
		return ErrInvalidContentMap
	}
	for index, section := range snapshot.Sections {
		excludedNodes := make(map[int64]bool, len(definition.Sections[index].Exclude))
		for _, pin := range definition.Sections[index].Exclude {
			entry, err := s.resolveMapPinTx(ctx, tx, access, pin)
			if err != nil {
				return ErrNotFound
			}
			excludedNodes[entry.Member.NodeID] = true
		}
		for _, pin := range append(slices.Clone(definition.Sections[index].HeadingSources),
			definition.Sections[index].DescriptionSources...) {
			if _, err := s.resolveMapPinTx(ctx, tx, access, pin); err != nil {
				return ErrNotFound
			}
		}
		for _, entry := range section.Entries {
			if entry.Availability == mapAvailabilityAvailable && excludedNodes[entry.Member.NodeID] {
				return ErrInvalidContentMap
			}
			if entry.Availability == mapAvailabilityUnavailable {
				versionID := entry.RequestedContentVersionID
				if entry.Passage != nil {
					var err error
					versionID, err = s.mapPassageIdentityTx(ctx, tx, entry)
					if err != nil {
						return ErrNotFound
					}
				}
				if entry.PinMode != document.MapPinVersionPinned &&
					(!access.AllSources || entry.PinMode != document.MapPinFollowCurrent) ||
					!mapPermits(access, versionID) {
					return ErrNotFound
				}
				continue
			}
			if entry.Passage != nil {
				pin := document.ContentMapPin{DocumentUID: entry.DocumentUID, Mode: entry.PinMode,
					ContentVersionID: entry.RequestedContentVersionID, Passage: entry.Passage}
				resolved, err := s.resolveMapPinTx(ctx, tx, access, pin)
				if err != nil || resolved.Member != entry.Member {
					return ErrNotFound
				}
				continue
			}
			if !mapPermits(access, entry.Member.ContentVersionID) {
				return ErrNotFound
			}
			node, err := nodeByIDTx(tx, entry.Member.NodeID)
			if err != nil || node.IsDir() || node.TrashedAt != nil {
				return ErrNotFound
			}
			if (entry.PinMode == document.MapPinFollowCurrent || entry.PinMode == "") &&
				node.CurrentVersionID != entry.Member.ContentVersionID {
				return ErrStaleRevision
			}
			version, err := scanContentVersion(tx.QueryRowContext(ctx, `SELECT `+contentVersionCols+`
				FROM content_versions WHERE version_id=? AND node_id=?`, entry.Member.ContentVersionID, entry.Member.NodeID))
			if err != nil {
				return ErrNotFound
			}
			memberRevision := version.NodeRevision
			if entry.PinMode == "" {
				// Query snapshots fence the live node revision. Pins instead
				// resolve their retained version's creation revision.
				memberRevision = node.Revision
			}
			if (SnapshotMember{NodeID: entry.Member.NodeID, ContentVersionID: version.ID,
				BlobHash: version.BlobHash, Size: version.Size, Revision: memberRevision}) != entry.Member {
				return ErrNotFound
			}
		}
	}
	return nil
}

// ContentMapSnapshotByID returns the exact frozen view only to a caller that
// still has access to every frozen version. Hidden members reveal no counts.
func (s *Store) ContentMapSnapshotByID(ctx context.Context, access MapAccess, id string) (ContentMapSnapshot, error) {
	if err := validateMapAccess(access); err != nil {
		return ContentMapSnapshot{}, err
	}
	if validateUUIDv4(id) != nil {
		return ContentMapSnapshot{}, ErrNotFound
	}
	var encoded []byte
	var owner, memberHash, scopeDigest string
	err := s.db.QueryRowContext(ctx, `SELECT snapshot_json,owner,member_hash,scope_digest FROM content_map_snapshots WHERE id=? AND owner=?`,
		id, access.Owner).Scan(&encoded, &owner, &memberHash, &scopeDigest)
	if err != nil {
		return ContentMapSnapshot{}, ErrNotFound
	}
	if scopeDigest != mapScopeDigest(access) {
		return ContentMapSnapshot{}, ErrNotFound
	}
	if len(encoded) > document.MaxContentMapMetadataBytes {
		return ContentMapSnapshot{}, ErrInvalidContentMap
	}
	var stored contentMapSnapshotStorage
	if err := json.Unmarshal(encoded, &stored); err != nil {
		return ContentMapSnapshot{}, ErrInvalidContentMap
	}
	snapshot := stored.ContentMapSnapshot
	if snapshot.ID != id || snapshot.Owner != owner ||
		snapshot.MemberHash != memberHash || snapshot.ScopeDigest != scopeDigest {
		return ContentMapSnapshot{}, ErrInvalidContentMap
	}
	if err := ValidateContentMapSnapshotRecord(snapshot); err != nil {
		return ContentMapSnapshot{}, err
	}
	if stored.Dependencies == nil || len(*stored.Dependencies) > document.MaxContentMapPins {
		return ContentMapSnapshot{}, ErrInvalidContentMap
	}
	for _, pin := range *stored.Dependencies {
		if _, err := document.ContentMapPinIdentity(pin); err != nil {
			return ContentMapSnapshot{}, ErrInvalidContentMap
		}
		if _, err := s.resolveMapPin(ctx, access, pin); err != nil {
			return ContentMapSnapshot{}, ErrNotFound
		}
	}
	for _, section := range snapshot.Sections {
		for _, entry := range section.Entries {
			versionID := entry.Member.ContentVersionID
			if entry.Passage != nil {
				var err error
				versionID, err = s.mapPassageIdentity(ctx, entry)
				if err != nil {
					return ContentMapSnapshot{}, ErrNotFound
				}
			}
			if entry.Availability == mapAvailabilityUnavailable && entry.PinMode == document.MapPinVersionPinned {
				if entry.Passage == nil {
					versionID = entry.RequestedContentVersionID
				}
			}
			if !mapPermits(access, versionID) {
				return ContentMapSnapshot{}, ErrNotFound
			}
			if entry.Availability == mapAvailabilityAvailable && entry.Passage != nil {
				authority, err := s.ResolvePassageAuthority(ctx, *entry.Passage)
				if err != nil || authority.Node.ID != entry.Member.NodeID ||
					authority.Version.ID != entry.Member.ContentVersionID ||
					authority.Version.BlobHash != entry.Member.BlobHash ||
					authority.Version.Size != entry.Member.Size ||
					authority.Version.NodeRevision != entry.Member.Revision {
					return ContentMapSnapshot{}, ErrNotFound
				}
			}
		}
	}
	return snapshot, nil
}

// ArchiveContentMap hides a definition without deleting retained snapshots.
func (s *Store) ArchiveContentMap(ctx context.Context, access MapAccess, id string, expectedRevision int64) (ContentMap, error) {
	if err := validateMapAccess(access); err != nil {
		return ContentMap{}, err
	}
	if validateUUIDv4(id) != nil {
		return ContentMap{}, ErrNotFound
	}
	var archived ContentMap
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		current, err := scanContentMap(tx.QueryRowContext(ctx, `SELECT id,owner,revision,definition_json,definition_digest,created_at,updated_at,COALESCE(archived_at,'')
			FROM content_maps WHERE id=? AND owner=?`, id, access.Owner))
		if err != nil {
			return err
		}
		if current.ArchivedAt != "" {
			return ErrNotFound
		}
		if current.Revision != expectedRevision {
			return ErrStaleRevision
		}
		archived = current
		archived.Revision++
		archived.ArchivedAt = nowRFC3339()
		archived.UpdatedAt = archived.ArchivedAt
		result, err := tx.ExecContext(ctx, `UPDATE content_maps SET revision=?,archived_at=?,updated_at=? WHERE id=? AND owner=? AND revision=? AND archived_at IS NULL`,
			archived.Revision, archived.ArchivedAt, archived.UpdatedAt, id, access.Owner, expectedRevision)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return ErrStaleRevision
		}
		return nil
	})
	return archived, err
}
