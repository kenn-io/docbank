// Package maps renders already-authorized structured maps for bounded reads.
package maps

import (
	"errors"
	"fmt"
	"html"
	"strings"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/query"
	"go.kenn.io/docbank/internal/store"
)

const (
	MaxMapReadBytes   = 32 << 10
	MaxMapReadEntries = 100
)

var ErrInvalidMapRead = errors.New("invalid map read window")

type ReadSection struct {
	ID          string                   `json:"id"`
	Heading     string                   `json:"heading"`
	Description string                   `json:"description"`
	Selector    *query.Query             `json:"selector,omitzero"`
	Include     []document.ContentMapPin `json:"include,omitzero"`
	Exclude     []document.ContentMapPin `json:"exclude,omitzero"`
	Entries     []ReadEntry              `json:"entries"`
}

// ReadEntry omits mutable file metadata; source chronology is not inferred
// from a snapshot's display-time path or modified timestamp.
type ReadEntry struct {
	DocumentUID               string                 `json:"document_uid"`
	Availability              string                 `json:"availability"`
	RequestedContentVersionID string                 `json:"requested_content_version_id,omitzero"`
	Member                    store.SnapshotMember   `json:"member"`
	Name                      string                 `json:"name"`
	PinMode                   string                 `json:"pin_mode,omitzero"`
	Passage                   *document.PassageRefV1 `json:"passage,omitzero"`
}

func projectReadEntry(entry store.ContentMapSnapshotEntry) ReadEntry {
	return ReadEntry{DocumentUID: entry.DocumentUID, Availability: entry.Availability,
		RequestedContentVersionID: entry.RequestedContentVersionID, Member: entry.Member,
		Name: entry.Name, PinMode: entry.PinMode, Passage: entry.Passage}
}

// ReadView carries a bounded structured page and the same page as Markdown.
// For mixed definition/snapshot pages, offsets count ordered section headers
// and entries while entry counts remain counts of actual pins or members.
// The underlying snapshot is read and authorized before this pure renderer.
type ReadView struct {
	Kind                  string                 `json:"kind"`
	MapID                 string                 `json:"map_id"`
	MapRevision           int64                  `json:"map_revision"`
	Title                 string                 `json:"title,omitzero"`
	Scope                 string                 `json:"scope,omitzero"`
	DefinitionUpdated     string                 `json:"definition_updated,omitzero"`
	SnapshotID            string                 `json:"snapshot_id,omitzero"`
	BeforeSnapshotID      string                 `json:"before_snapshot_id,omitzero"`
	BeforeSnapshotCreated string                 `json:"before_snapshot_created,omitzero"`
	ScopeDigest           string                 `json:"scope_digest,omitzero"`
	SnapshotCreated       string                 `json:"snapshot_created,omitzero"`
	Markdown              string                 `json:"markdown"`
	Freshness             string                 `json:"freshness"`
	Coverage              string                 `json:"coverage"`
	Sections              []ReadSection          `json:"sections"`
	Delta                 *store.ContentMapDelta `json:"delta,omitzero"`
	TotalSections         int                    `json:"total_sections"`
	TotalEntries          int                    `json:"total_entries"`
	AvailableEntries      int                    `json:"available_entries"`
	Offset                int                    `json:"offset"`
	OffsetUnit            string                 `json:"offset_unit"`
	ReturnedEntries       int                    `json:"returned_entries"`
	NextOffset            int                    `json:"next_offset"`
	Truncated             bool                   `json:"truncated"`
}

func validWindow(offset, limit int) bool {
	return offset >= 0 && limit >= 1 && limit <= MaxMapReadEntries
}

// RenderDefinition pages curated sections and explicit pins in stable order.
// Query selectors remain structured JSON, never interpreted as prose.
func RenderDefinition(record store.ContentMap, offset, limit int) (ReadView, error) {
	if !validWindow(offset, limit) {
		return ReadView{}, ErrInvalidMapRead
	}
	view := ReadView{Kind: "definition", MapID: record.ID, MapRevision: record.Revision,
		Freshness: "current_definition", Coverage: "authorized_page",
		OffsetUnit: "pin",
		Title:      record.Definition.Title, Scope: record.Definition.Scope,
		DefinitionUpdated: record.UpdatedAt, Offset: offset,
		TotalSections: len(record.Definition.Sections), Sections: make([]ReadSection, 0)}
	for _, section := range record.Definition.Sections {
		view.TotalEntries += len(section.Include) + len(section.Exclude)
	}
	view.AvailableEntries = view.TotalEntries
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\nMap: %s · revision: %d\n\nScope: %s\n\nDefinition updated: %s\n\n",
		escapeMarkdown(record.Definition.Title, 256), record.ID, record.Revision,
		escapeMarkdown(record.Definition.Scope, 256), escapeMarkdown(record.UpdatedAt, 64))
	if view.TotalEntries == 0 {
		view.OffsetUnit = "section"
		for index, section := range record.Definition.Sections {
			if index < offset {
				continue
			}
			var block strings.Builder
			fmt.Fprintf(&block, "## %s\n\n", escapeMarkdown(section.Heading, 256))
			if section.Description != "" {
				fmt.Fprintf(&block, "%s\n\n", escapeMarkdown(section.Description, 512))
			}
			if section.Selector != nil {
				block.WriteString("Selector: structured query (see JSON view)\n\n")
			}
			if len(view.Sections) >= limit || b.Len()+block.Len() > MaxMapReadBytes {
				view.NextOffset = index
				view.Truncated = true
				break
			}
			b.WriteString(block.String())
			view.Sections = append(view.Sections, ReadSection{ID: section.ID, Heading: section.Heading,
				Description: section.Description, Selector: section.Selector})
		}
		view.Markdown = b.String()
		return view, nil
	}
	view.OffsetUnit = "item"
	ordinal, slots := 0, 0
	stopped := false
	for _, section := range record.Definition.Sections {
		sectionOffset := ordinal
		ordinal++
		page := ReadSection{ID: section.ID, Heading: section.Heading,
			Description: section.Description, Selector: section.Selector,
			Include: make([]document.ContentMapPin, 0), Exclude: make([]document.ContentMapPin, 0)}
		var block strings.Builder
		fmt.Fprintf(&block, "## %s\n\n", escapeMarkdown(section.Heading, 256))
		if section.Description != "" {
			fmt.Fprintf(&block, "%s\n\n", escapeMarkdown(section.Description, 512))
		}
		if section.Selector != nil {
			block.WriteString("Selector: structured query (see JSON view)\n\n")
		}
		if len(section.Include)+len(section.Exclude) == 0 {
			if sectionOffset < offset {
				continue
			}
			if slots >= limit || b.Len()+block.Len() > MaxMapReadBytes {
				view.NextOffset, stopped = sectionOffset, true
				break
			}
			b.WriteString(block.String())
			view.Sections = append(view.Sections, page)
			slots++
			continue
		}
		shown := false
		for _, group := range []struct {
			kind string
			pins []document.ContentMapPin
		}{{"include", section.Include}, {"exclude", section.Exclude}} {
			for _, pin := range group.pins {
				pinOffset := ordinal
				ordinal++
				if pinOffset < offset {
					continue
				}
				if slots >= limit {
					view.NextOffset, stopped = pinOffset, true
					break
				}
				line := mapPinMarkdown(group.kind, pin)
				if b.Len()+block.Len()+len(line) > MaxMapReadBytes {
					view.NextOffset, stopped = pinOffset, true
					break
				}
				block.WriteString(line)
				if group.kind == "include" {
					page.Include = append(page.Include, pin)
				} else {
					page.Exclude = append(page.Exclude, pin)
				}
				view.ReturnedEntries++
				slots++
				shown = true
			}
			if stopped {
				break
			}
		}
		if shown {
			b.WriteString(block.String())
			view.Sections = append(view.Sections, page)
		}
		if stopped {
			break
		}
	}
	view.Truncated = stopped
	view.Markdown = b.String()
	return view, nil
}

func mapPinMarkdown(kind string, pin document.ContentMapPin) string {
	line := fmt.Sprintf("- %s %s · document `%s`", kind, pin.Mode, pin.DocumentUID)
	if pin.ContentVersionID != "" {
		line += fmt.Sprintf(" · exact version `%s`", pin.ContentVersionID)
	}
	if pin.Passage != nil {
		line += " · passage " + passageCitation(pin.Passage)
	}
	return line + "\n"
}

// RenderSnapshot pages sections and entries in stable order. Snapshot creation
// is labeled separately from source metadata; no source chronology is inferred.
func RenderSnapshot(snapshot store.ContentMapSnapshot, offset, limit int) (ReadView, error) {
	if !validWindow(offset, limit) {
		return ReadView{}, ErrInvalidMapRead
	}
	view := ReadView{Kind: "snapshot", MapID: snapshot.MapID, MapRevision: snapshot.MapRevision,
		Freshness: "frozen_snapshot", Coverage: "authorized_page",
		OffsetUnit: "entry",
		SnapshotID: snapshot.ID, ScopeDigest: snapshot.ScopeDigest,
		SnapshotCreated: snapshot.CreatedAt, Offset: offset, TotalSections: len(snapshot.Sections),
		Sections: make([]ReadSection, 0)}
	for _, section := range snapshot.Sections {
		view.TotalEntries += len(section.Entries)
	}
	view.AvailableEntries = view.TotalEntries
	var b strings.Builder
	fmt.Fprintf(&b, "# Map snapshot %s\n\nMap: %s · revision: %d\n\nSnapshot created: %s\n\nScope: %s\n\n",
		snapshot.ID, snapshot.MapID, snapshot.MapRevision,
		escapeMarkdown(snapshot.CreatedAt, 64), escapeMarkdown(snapshot.ScopeDigest, 80))
	if view.TotalEntries == 0 {
		view.OffsetUnit = "section"
		for index, section := range snapshot.Sections {
			if index < offset {
				continue
			}
			var block strings.Builder
			fmt.Fprintf(&block, "## %s\n\n", escapeMarkdown(section.Heading, 256))
			if section.Description != "" {
				fmt.Fprintf(&block, "%s\n\n", escapeMarkdown(section.Description, 512))
			}
			if len(view.Sections) >= limit || b.Len()+block.Len() > MaxMapReadBytes {
				view.NextOffset = index
				view.Truncated = true
				break
			}
			b.WriteString(block.String())
			view.Sections = append(view.Sections, ReadSection{ID: section.ID, Heading: section.Heading,
				Description: section.Description, Entries: make([]ReadEntry, 0)})
		}
		view.Markdown = b.String()
		return view, nil
	}
	view.OffsetUnit = "item"
	ordinal, slots := 0, 0
	stopped := false
	for _, section := range snapshot.Sections {
		sectionOffset := ordinal
		ordinal++
		page := ReadSection{ID: section.ID, Heading: section.Heading,
			Description: section.Description, Entries: make([]ReadEntry, 0)}
		var block strings.Builder
		fmt.Fprintf(&block, "## %s\n\n", escapeMarkdown(section.Heading, 256))
		if section.Description != "" {
			fmt.Fprintf(&block, "%s\n\n", escapeMarkdown(section.Description, 512))
		}
		if len(section.Entries) == 0 {
			if sectionOffset < offset {
				continue
			}
			if slots >= limit || b.Len()+block.Len() > MaxMapReadBytes {
				view.NextOffset, stopped = sectionOffset, true
				break
			}
			b.WriteString(block.String())
			view.Sections = append(view.Sections, page)
			slots++
			continue
		}
		sectionShown := false
		for _, entry := range section.Entries {
			entryOffset := ordinal
			ordinal++
			if entryOffset < offset {
				continue
			}
			if slots >= limit {
				view.NextOffset, stopped = entryOffset, true
				break
			}
			line := snapshotEntryMarkdown(entry)
			if b.Len()+block.Len()+len(line) > MaxMapReadBytes {
				view.NextOffset, stopped = entryOffset, true
				break
			}
			block.WriteString(line)
			page.Entries = append(page.Entries, projectReadEntry(entry))
			view.ReturnedEntries++
			slots++
			sectionShown = true
		}
		if sectionShown {
			b.WriteString(block.String())
			view.Sections = append(view.Sections, page)
		}
		if stopped {
			break
		}
	}
	view.Truncated = stopped
	view.Markdown = b.String()
	return view, nil
}

// RenderDelta pages only the bounded change lists. Counts remain the full
// authorized populations even if the underlying delta was truncated.
func RenderDelta(delta store.ContentMapDelta, offset, limit int) (ReadView, error) {
	if !validWindow(offset, limit) {
		return ReadView{}, ErrInvalidMapRead
	}
	view := ReadView{Kind: "delta", MapID: delta.MapID, SnapshotID: delta.AfterSnapshotID,
		BeforeSnapshotID: delta.BeforeSnapshotID, BeforeSnapshotCreated: delta.BeforeSnapshotCreated,
		ScopeDigest: delta.ScopeDigest, SnapshotCreated: delta.AfterSnapshotCreated,
		Freshness: "snapshot_comparison", Coverage: "authorized_page",
		OffsetUnit: "event",
		Offset:     offset, Delta: &delta, Sections: make([]ReadSection, 0)}
	view.TotalEntries = delta.Counts.Added + delta.Counts.Removed + delta.Counts.VersionChanged +
		delta.Counts.Reordered + delta.Counts.Unavailable
	view.AvailableEntries = len(delta.Added) + len(delta.Removed) + len(delta.VersionChanged) +
		len(delta.Reordered) + len(delta.Unavailable)
	var b strings.Builder
	fmt.Fprintf(&b, "# Map refresh delta\n\nMap: %s\n\nScope digest: %s\n\nBefore snapshot: %s\n\nBefore snapshot created: %s\n\nAfter snapshot: %s\n\nAfter snapshot created: %s\n\n"+
		"Definition changed: %s\n\nChanges: %d added, %d removed, %d version changed, %d reordered, %d unavailable.\n\n",
		delta.MapID, escapeMarkdown(delta.ScopeDigest, 80), delta.BeforeSnapshotID,
		escapeMarkdown(delta.BeforeSnapshotCreated, 64), delta.AfterSnapshotID,
		escapeMarkdown(delta.AfterSnapshotCreated, 64), yesNo(delta.DefinitionChanged),
		delta.Counts.Added, delta.Counts.Removed, delta.Counts.VersionChanged,
		delta.Counts.Reordered, delta.Counts.Unavailable)
	ordinal := 0
	stopped := false
	for _, group := range []struct {
		name    string
		changes []store.ContentMapDeltaChange
	}{{"added", delta.Added}, {"removed", delta.Removed}, {"version changed", delta.VersionChanged},
		{"reordered", delta.Reordered}, {"unavailable", delta.Unavailable}} {
		for _, change := range group.changes {
			if ordinal < offset {
				ordinal++
				continue
			}
			if view.ReturnedEntries >= limit {
				stopped = true
				break
			}
			line := fmt.Sprintf("- %s · section %s · document `%s` · before `%s` at %d · after `%s` at %d",
				group.name, escapeMarkdown(change.SectionID, 64), change.DocumentUID,
				change.BeforeContentVersionID, change.BeforeIndex,
				change.AfterContentVersionID, change.AfterIndex)
			if change.PassageID != "" {
				line += " · passage `" + escapeMarkdown(change.PassageID, 64) + "`"
			}
			line += "\n"
			if b.Len()+len(line) > MaxMapReadBytes {
				stopped = true
				break
			}
			b.WriteString(line)
			view.ReturnedEntries++
			ordinal++
		}
		if stopped {
			break
		}
	}
	if offset+view.ReturnedEntries < view.AvailableEntries {
		view.NextOffset = offset + view.ReturnedEntries
	}
	view.Truncated = delta.Truncated || view.NextOffset != 0
	view.Markdown = b.String()
	return view, nil
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func snapshotEntryMarkdown(entry store.ContentMapSnapshotEntry) string {
	if entry.Availability == "unavailable" {
		return fmt.Sprintf("- unavailable pin · document `%s` · requested version `%s`\n",
			entry.DocumentUID, entry.RequestedContentVersionID)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "- %s · document `%s` · exact version `%s` · node `%d`\n",
		escapeMarkdown(entry.Name, 256), entry.DocumentUID, entry.Member.ContentVersionID, entry.Member.NodeID)
	if entry.Passage != nil {
		fmt.Fprintf(&b, "  - Passage: %s\n", passageCitation(entry.Passage))
	}
	return b.String()
}

func passageCitation(ref *document.PassageRefV1) string {
	value := fmt.Sprintf("vault `%s` · document `%s` · version `%s` · source `%s` · build `%s` · rendition `%s` · body `%s` · bytes %d–%d · quote `%s`",
		ref.VaultUID, ref.DocumentUID, ref.ContentVersionID, ref.SourceSHA256,
		ref.RenditionBuildID, ref.AttachmentID, ref.BodySHA256, ref.ByteStart, ref.ByteEnd, ref.QuoteSHA256)
	if ref.FederationDomainUID != "" {
		value += fmt.Sprintf(" · domain `%s`", ref.FederationDomainUID)
	}
	return value
}

func escapeMarkdown(value string, maxRunes int) string {
	value = strings.Join(strings.Fields(value), " ")
	characters := []rune(value)
	if len(characters) > maxRunes {
		value = string(characters[:maxRunes]) + "…"
	}
	value = html.EscapeString(value)
	return strings.NewReplacer("\\", "\\\\", "`", "\\`", "*", "\\*", "_", "\\_",
		"[", "\\[", "]", "\\]", "(", "\\(", ")", "\\)", "#", "\\#",
		"!", "\\!", "|", "\\|", ">", "\\>").Replace(value)
}
