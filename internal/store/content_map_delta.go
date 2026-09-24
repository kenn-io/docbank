package store

import (
	"context"

	"go.kenn.io/docbank/document"
)

const MaxContentMapDeltaEntries = 100

// ContentMapDeltaByIDs reads both immutable snapshots under the same caller
// scope before comparing them. Cross-map comparisons have no visible result.
func (s *Store) ContentMapDeltaByIDs(ctx context.Context, access MapAccess,
	beforeID, afterID string,
) (ContentMapDelta, error) {
	before, err := s.ContentMapSnapshotByID(ctx, access, beforeID)
	if err != nil {
		return ContentMapDelta{}, err
	}
	after, err := s.ContentMapSnapshotByID(ctx, access, afterID)
	if err != nil {
		return ContentMapDelta{}, err
	}
	if before.MapID != after.MapID {
		return ContentMapDelta{}, ErrNotFound
	}
	return diffContentMapSnapshots(before, after), nil
}

// ContentMapDeltaChange identifies one source in one stable section. Positions
// are zero-based; -1 means the source was absent from that snapshot.
type ContentMapDeltaChange struct {
	SectionID              string `json:"section_id"`
	DocumentUID            string `json:"document_uid"`
	PassageID              string `json:"passage_id,omitzero"`
	BeforeContentVersionID string `json:"before_content_version_id,omitzero"`
	AfterContentVersionID  string `json:"after_content_version_id,omitzero"`
	BeforeIndex            int    `json:"before_index"`
	AfterIndex             int    `json:"after_index"`
}

type ContentMapDeltaCounts struct {
	Added          int `json:"added"`
	Removed        int `json:"removed"`
	VersionChanged int `json:"version_changed"`
	Reordered      int `json:"reordered"`
	Unavailable    int `json:"unavailable"`
}

// ContentMapDelta compares two already-authorized immutable snapshots. Lists
// are bounded independently while counts describe their complete populations.
type ContentMapDelta struct {
	MapID                 string                  `json:"map_id"`
	ScopeDigest           string                  `json:"scope_digest"`
	BeforeSnapshotID      string                  `json:"before_snapshot_id,omitzero"`
	BeforeSnapshotCreated string                  `json:"before_snapshot_created,omitzero"`
	AfterSnapshotID       string                  `json:"after_snapshot_id"`
	AfterSnapshotCreated  string                  `json:"after_snapshot_created"`
	DefinitionChanged     bool                    `json:"definition_changed"`
	Changed               bool                    `json:"changed"`
	Truncated             bool                    `json:"truncated"`
	Counts                ContentMapDeltaCounts   `json:"counts"`
	Added                 []ContentMapDeltaChange `json:"added"`
	Removed               []ContentMapDeltaChange `json:"removed"`
	VersionChanged        []ContentMapDeltaChange `json:"version_changed"`
	Reordered             []ContentMapDeltaChange `json:"reordered"`
	Unavailable           []ContentMapDeltaChange `json:"unavailable"`
}

type locatedMapEntry struct {
	entry ContentMapSnapshotEntry
	index int
}

type mapEntryKey struct {
	sectionID, documentUID string
	passageRef             document.PassageRefV1
	hasPassage             bool
}

func mapSnapshotEntryKey(sectionID string, entry ContentMapSnapshotEntry) mapEntryKey {
	key := mapEntryKey{sectionID: sectionID, documentUID: entry.DocumentUID}
	if entry.Passage != nil {
		key.passageRef, key.hasPassage = *entry.Passage, true
	}
	return key
}

func mapPassageID(ref *document.PassageRefV1) string {
	if ref == nil {
		return ""
	}
	id, _ := document.PassageIdentityV1(*ref) // Stored snapshots already validate passage identity.
	return id
}

func diffContentMapSnapshots(before, after ContentMapSnapshot) ContentMapDelta {
	delta := ContentMapDelta{MapID: after.MapID, ScopeDigest: after.ScopeDigest,
		BeforeSnapshotID: before.ID, BeforeSnapshotCreated: before.CreatedAt,
		AfterSnapshotID: after.ID, AfterSnapshotCreated: after.CreatedAt,
		DefinitionChanged: before.ID != "" && (before.MapRevision != after.MapRevision ||
			before.DefinitionDigest != after.DefinitionDigest),
		Added: []ContentMapDeltaChange{}, Removed: []ContentMapDeltaChange{},
		VersionChanged: []ContentMapDeltaChange{}, Reordered: []ContentMapDeltaChange{},
		Unavailable: []ContentMapDeltaChange{}}
	old := make(map[mapEntryKey]locatedMapEntry)
	current := make(map[mapEntryKey]bool)
	for _, section := range before.Sections {
		for index, entry := range section.Entries {
			old[mapSnapshotEntryKey(section.ID, entry)] = locatedMapEntry{entry: entry, index: index}
		}
	}
	for _, section := range after.Sections {
		for index, entry := range section.Entries {
			key := mapSnapshotEntryKey(section.ID, entry)
			current[key] = true
			prior, existed := old[key]
			change := mapDeltaChange(section.ID, entry.DocumentUID, prior, index, entry, existed)
			if entry.Availability == mapAvailabilityUnavailable {
				if !existed || prior.entry.Availability != mapAvailabilityUnavailable {
					appendMapDeltaChange(&delta.Unavailable, &delta.Counts.Unavailable, &delta.Truncated, change)
				}
				continue
			}
			if !existed || prior.entry.Availability == mapAvailabilityUnavailable {
				appendMapDeltaChange(&delta.Added, &delta.Counts.Added, &delta.Truncated, change)
				continue
			}
			if prior.entry.Member.ContentVersionID != entry.Member.ContentVersionID {
				appendMapDeltaChange(&delta.VersionChanged, &delta.Counts.VersionChanged, &delta.Truncated, change)
			}
			if prior.index != index {
				appendMapDeltaChange(&delta.Reordered, &delta.Counts.Reordered, &delta.Truncated, change)
			}
		}
	}
	for _, section := range before.Sections {
		for index, entry := range section.Entries {
			key := mapSnapshotEntryKey(section.ID, entry)
			if current[key] {
				continue
			}
			appendMapDeltaChange(&delta.Removed, &delta.Counts.Removed, &delta.Truncated,
				ContentMapDeltaChange{SectionID: section.ID, DocumentUID: entry.DocumentUID,
					PassageID:              mapPassageID(entry.Passage),
					BeforeContentVersionID: entry.Member.ContentVersionID, BeforeIndex: index, AfterIndex: -1})
		}
	}
	delta.Changed = delta.Changed || delta.DefinitionChanged || delta.Counts.Added > 0 ||
		delta.Counts.Removed > 0 || delta.Counts.VersionChanged > 0 ||
		delta.Counts.Reordered > 0 || delta.Counts.Unavailable > 0
	return delta
}

func mapDeltaChange(sectionID, documentUID string, before locatedMapEntry, afterIndex int,
	after ContentMapSnapshotEntry, existed bool,
) ContentMapDeltaChange {
	change := ContentMapDeltaChange{SectionID: sectionID, DocumentUID: documentUID,
		PassageID:   mapPassageID(after.Passage),
		BeforeIndex: -1, AfterIndex: afterIndex, AfterContentVersionID: after.Member.ContentVersionID}
	if existed {
		change.BeforeIndex = before.index
		change.BeforeContentVersionID = before.entry.Member.ContentVersionID
	}
	return change
}

func appendMapDeltaChange(list *[]ContentMapDeltaChange, count *int, truncated *bool,
	change ContentMapDeltaChange,
) {
	(*count)++
	if len(*list) < MaxContentMapDeltaEntries {
		*list = append(*list, change)
	} else {
		*truncated = true
	}
}
