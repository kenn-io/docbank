package store

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"slices"

	"go.kenn.io/docbank/document"
)

const conceptFederationExtensionVersion = 1
const maxConceptFederationExtensionBytes = 16 << 20

var ErrConceptFederationAuthority = errors.New("concept federation export requires full source authority")

// ConceptFederationExtension is a versioned logical snapshot for future
// federation recovery. Origin and UUIDs stay explicit: equal names from
// different vaults never imply equal tag identities.
type ConceptFederationExtension struct {
	Version        int                         `json:"version"`
	DomainUID      string                      `json:"domain_uid"`
	OriginVaultUID string                      `json:"origin_vault_uid"`
	Tags           []ConceptFederationTag      `json:"tags"`
	Concepts       []ConceptFederationDetail   `json:"concepts"`
	Aliases        []ConceptFederationAlias    `json:"aliases"`
	Edges          []ConceptFederationEdge     `json:"edges"`
	Redirects      []ConceptFederationRedirect `json:"redirects"`
	MergeAudits    []ConceptFederationMerge    `json:"merge_audits"`
	PassageTags    []ConceptFederationPassage  `json:"passage_tags"`
}

type ConceptFederationTag struct {
	TagID    string `json:"tag_id"`
	Name     string `json:"name"`
	Revision int64  `json:"revision"`
}

type ConceptFederationDetail struct {
	TagID       string `json:"tag_id"`
	Description string `json:"description"`
	Revision    int64  `json:"revision"`
}

type ConceptFederationAlias struct {
	Alias string `json:"alias"`
	TagID string `json:"tag_id"`
}

type ConceptFederationEdge struct {
	ParentTagID string `json:"parent_tag_id"`
	ChildTagID  string `json:"child_tag_id"`
	Kind        string `json:"kind"`
}

type ConceptFederationRedirect struct {
	SourceTagID string `json:"source_tag_id"`
	TargetTagID string `json:"target_tag_id"`
	SourceName  string `json:"source_name"`
	MergedAt    string `json:"merged_at"`
	MergeID     string `json:"merge_id"`
}

type ConceptFederationMerge struct {
	MergeID        string  `json:"merge_id"`
	SourceTagID    string  `json:"source_tag_id"`
	TargetTagID    string  `json:"target_tag_id"`
	SourceRevision int64   `json:"source_revision"`
	TargetRevision int64   `json:"target_revision"`
	PreviewJSON    []byte  `json:"preview_json" format:"byte"`
	CommittedAt    string  `json:"committed_at"`
	ReversedAt     *string `json:"reversed_at"`
}

type ConceptFederationPassage struct {
	PassageID string                `json:"passage_id"`
	TagID     string                `json:"tag_id"`
	Ref       document.PassageRefV1 `json:"ref"`
}

// ExportConceptFederationExtension requires explicit full authority because a
// whole-vault vocabulary reveals names, relationships and passage assignments.
// Scoped federation needs a separately reviewed source-set projection.
func (s *Store) ExportConceptFederationExtension(ctx context.Context, domainUID string, fullAuthority bool) (result ConceptFederationExtension, err error) {
	if !fullAuthority {
		return result, ErrConceptFederationAuthority
	}
	if err := validateUUIDv4(domainUID); err != nil {
		return result, fmt.Errorf("invalid federation domain UID: %w", err)
	}
	snapshot, err := s.BeginMetadataSnapshot(ctx)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, snapshot.Close()) }()
	if err := snapshot.Export(ctx, io.Discard); err != nil {
		return result, err
	}
	result = ConceptFederationExtension{Version: conceptFederationExtensionVersion,
		DomainUID: domainUID, OriginVaultUID: s.vaultID,
		Tags: []ConceptFederationTag{}, Concepts: []ConceptFederationDetail{},
		Aliases: []ConceptFederationAlias{}, Edges: []ConceptFederationEdge{},
		Redirects: []ConceptFederationRedirect{}, MergeAudits: []ConceptFederationMerge{},
		PassageTags: []ConceptFederationPassage{}}
	if err := exportTags(ctx, snapshot, func(value any) error {
		tag, ok := value.(metadataTag)
		if !ok {
			return fmt.Errorf("unsupported tag federation record %T", value)
		}
		result.Tags = append(result.Tags, ConceptFederationTag{TagID: tag.ID, Name: tag.Name, Revision: tag.Revision})
		return nil
	}); err != nil {
		return ConceptFederationExtension{}, err
	}
	if err := exportTagConceptMetadata(ctx, snapshot, func(value any) error {
		switch row := value.(type) {
		case metadataTagConcept:
			result.Concepts = append(result.Concepts, ConceptFederationDetail{row.TagID, row.Description, row.Revision})
		case metadataTagAlias:
			result.Aliases = append(result.Aliases, ConceptFederationAlias{row.Alias, row.TagID})
		case metadataTagConceptEdge:
			result.Edges = append(result.Edges, ConceptFederationEdge{row.ParentTagID, row.ChildTagID, row.Kind})
		case metadataTagRedirect:
			result.Redirects = append(result.Redirects, ConceptFederationRedirect{row.SourceTagID, row.TargetTagID, row.SourceName, row.MergedAt, row.MergeID})
		case metadataTagMergeAudit:
			result.MergeAudits = append(result.MergeAudits, ConceptFederationMerge{row.MergeID, row.SourceTagID,
				row.TargetTagID, row.SourceRevision, row.TargetRevision, row.PreviewJSON, row.CommittedAt, row.ReversedAt})
		default:
			return fmt.Errorf("unsupported concept federation record %T", value)
		}
		return nil
	}); err != nil {
		return ConceptFederationExtension{}, err
	}
	if err := exportPassageTagMetadata(ctx, snapshot, func(value any) error {
		row, ok := value.(metadataPassageTag)
		if !ok {
			return fmt.Errorf("unsupported passage federation record %T", value)
		}
		var ref document.PassageRefV1
		if err := json.Unmarshal(row.RefJSON, &ref); err != nil {
			return err
		}
		result.PassageTags = append(result.PassageTags, ConceptFederationPassage{row.PassageID, row.TagID, ref})
		return nil
	}); err != nil {
		return ConceptFederationExtension{}, err
	}
	if err := ValidateConceptFederationExtension(result, true); err != nil {
		return ConceptFederationExtension{}, err
	}
	return result, nil
}

// ValidateConceptFederationExtension checks the version and identity-bearing
// references before a recipient considers this optional federation payload.
func ValidateConceptFederationExtension(value ConceptFederationExtension, fullAuthority bool) error {
	if !fullAuthority {
		return ErrConceptFederationAuthority
	}
	if value.Version != conceptFederationExtensionVersion || validateUUIDv4(value.DomainUID) != nil ||
		validateUUIDv4(value.OriginVaultUID) != nil {
		return ErrInvalidTag
	}
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) > maxConceptFederationExtensionBytes {
		return ErrInvalidTag
	}
	tags := make(map[string]bool, len(value.Tags))
	names := make(map[string]bool, len(value.Tags)+len(value.Aliases))
	concepts := make(map[string]bool, len(value.Concepts))
	for _, tag := range value.Tags {
		name, err := NormalizeTagName(tag.Name)
		if validateUUIDv4(tag.TagID) != nil || err != nil || name != tag.Name || tag.Revision < 1 ||
			tags[tag.TagID] || names[tag.Name] {
			return ErrInvalidTag
		}
		tags[tag.TagID], names[tag.Name] = true, true
	}
	for _, concept := range value.Concepts {
		if !tags[concept.TagID] || concepts[concept.TagID] || !validConceptDescription(concept.Description) || concept.Revision < 1 {
			return ErrInvalidTag
		}
		concepts[concept.TagID] = true
	}
	for _, alias := range value.Aliases {
		name, err := NormalizeTagName(alias.Alias)
		if !tags[alias.TagID] || err != nil || name != alias.Alias || names[alias.Alias] {
			return ErrInvalidTag
		}
		names[alias.Alias] = true
	}
	broader := make(map[string][]string)
	edgeIDs := make(map[string]bool, len(value.Edges))
	for _, edge := range value.Edges {
		if !tags[edge.ParentTagID] || !tags[edge.ChildTagID] || edge.ParentTagID == edge.ChildTagID ||
			!slices.Contains([]string{"broader", "related"}, edge.Kind) {
			return ErrInvalidTag
		}
		key := edge.ParentTagID + "\x00" + edge.ChildTagID + "\x00" + edge.Kind
		if edgeIDs[key] {
			return ErrInvalidTag
		}
		edgeIDs[key] = true
		if edge.Kind == "broader" {
			if document.WouldCreateConceptCycle(broader, edge.ParentTagID, edge.ChildTagID) {
				return ErrCycle
			}
			broader[edge.ParentTagID] = append(broader[edge.ParentTagID], edge.ChildTagID)
		}
	}
	redirects := make(map[string]ConceptFederationRedirect, len(value.Redirects))
	for _, redirect := range value.Redirects {
		if validateUUIDv4(redirect.SourceTagID) != nil || !tags[redirect.TargetTagID] ||
			redirect.SourceTagID == redirect.TargetTagID || tags[redirect.SourceTagID] ||
			redirects[redirect.SourceTagID].SourceTagID != "" || validateUUIDv4(redirect.MergeID) != nil ||
			validateCanonicalConceptName(redirect.SourceName) != nil ||
			validateMetadataTime("tag redirect merged_at", redirect.MergedAt) != nil {
			return ErrInvalidTag
		}
		redirects[redirect.SourceTagID] = redirect
	}
	merges := make(map[string]ConceptFederationMerge, len(value.MergeAudits))
	mergeTarget := func(original string) string {
		if later, ok := redirects[original]; ok {
			return later.TargetTagID
		}
		return original
	}
	for _, merge := range value.MergeAudits {
		if merges[merge.MergeID].MergeID != "" || validateTagConceptMetadataRecord(metadataTagMergeAudit{
			Type: metadataTagMergeAuditType, MergeID: merge.MergeID, SourceTagID: merge.SourceTagID,
			TargetTagID: merge.TargetTagID, SourceRevision: merge.SourceRevision,
			TargetRevision: merge.TargetRevision, PreviewJSON: merge.PreviewJSON,
			CommittedAt: merge.CommittedAt, ReversedAt: merge.ReversedAt,
		}) != nil {
			return ErrInvalidTag
		}
		merges[merge.MergeID] = merge
		if merge.ReversedAt == nil {
			redirect := redirects[merge.SourceTagID]
			if redirect.MergeID != merge.MergeID || redirect.TargetTagID != mergeTarget(merge.TargetTagID) {
				return ErrInvalidTag
			}
		}
	}
	for _, redirect := range value.Redirects {
		merge := merges[redirect.MergeID]
		if merge.SourceTagID != redirect.SourceTagID ||
			mergeTarget(merge.TargetTagID) != redirect.TargetTagID || merge.ReversedAt != nil {
			return ErrInvalidTag
		}
	}
	passages := make(map[string]bool, len(value.PassageTags))
	for _, passage := range value.PassageTags {
		id, err := document.PassageIdentityV1(passage.Ref)
		if !tags[passage.TagID] || err != nil || id != passage.PassageID ||
			(passage.Ref.FederationDomainUID != "" && passage.Ref.FederationDomainUID != value.DomainUID) ||
			(passage.Ref.VaultUID != value.OriginVaultUID && passage.Ref.FederationDomainUID != value.DomainUID) ||
			passages[passage.PassageID+"\x00"+passage.TagID] {
			return ErrInvalidTag
		}
		passages[passage.PassageID+"\x00"+passage.TagID] = true
	}
	return nil
}
