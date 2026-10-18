package kataevidence

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strconv"
	"strings"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/kataclient"
)

type Context struct {
	Issue      kataclient.Issue `json:"issue"`
	Passages   []Resolution     `json:"passages"`
	NextOffset int              `json:"next_offset,omitzero"`
}

func documentValidate(p Selector) error { return document.ValidateTextCitation(*p.Citation) }

// Context reads both native and msgvault-delivered Docbank entries, never starts processing.
func (s Service) Context(ctx context.Context, r ContextRequest) (Context, error) {
	if r.Offset < 0 || r.Offset > MaxContextOffset || r.Issue == "" {
		return Context{}, ErrInvalid
	}
	issue, err := s.Kata.Get(ctx, r.Issue)
	if err != nil {
		return Context{}, err
	}
	var rawEntries []jsontext.Value
	for _, key := range []string{MetadataKey, MsgvaultMetadataKey} {
		if len(issue.Metadata[key]) == 0 {
			continue
		}
		limit, maxBytes := MaxEntries, 512<<10
		if key == MsgvaultMetadataKey {
			limit, maxBytes = 256, 2<<20
		}
		env, err := parseEnvelopeBound(issue.Metadata[key], limit, maxBytes)
		if err != nil {
			return Context{}, err
		}
		rawEntries = append(rawEntries, env.entries...)
	}
	result := Context{Issue: issue, Passages: []Resolution{}}
	quotes := savedQuotes(issue.Body)
	for _, comment := range issue.Comments {
		quotes = append(quotes, savedQuotes(comment.Body)...)
	}
	type contextEntry struct {
		Entry

		legacy      jsontext.Value
		unsupported bool
	}
	var entries []contextEntry
	for _, raw := range rawEntries {
		var discriminator struct {
			Reference struct {
				Kind string `json:"kind"`
			} `json:"reference"`
		}
		if json.Unmarshal(raw, &discriminator) != nil {
			entries = append(entries, contextEntry{unsupported: true})
			continue
		}
		switch discriminator.Reference.Kind {
		case "docbank":
			var e Entry
			decodeErr := json.Unmarshal(raw, &e)
			_, valid := parseNative(raw)
			entries = append(entries, contextEntry{Entry: e, unsupported: decodeErr != nil || !valid})
		case "docbank_rendition":
			var e struct {
				ID        string `json:"id"`
				Passage   string `json:"passage"`
				Reference struct {
					Version int                `json:"version"`
					Docbank *DocumentReference `json:"docbank_rendition"`
				} `json:"reference"`
				Pending *Pending `json:"pending"`
			}
			decodeErr := json.Unmarshal(raw, &e)
			ref := Reference{Version: e.Reference.Version, Kind: "docbank", Docbank: e.Reference.Docbank}
			entries = append(entries, contextEntry{ID: e.ID, Passage: e.Passage, Reference: ref, Pending: e.Pending, legacy: raw, unsupported: decodeErr != nil})
		}
	}
	start := min(r.Offset, len(entries))
	end := min(start+ContextPageSize, len(entries))
	for _, entry := range entries[start:end] {
		resolution := Resolution{State: "unreachable", Evidence: Evidence{ID: entry.ID, Passage: entry.Passage, Reference: entry.Reference, ContentTrust: "untrusted"}}
		if entry.unsupported || ValidateReference(entry.Reference) != nil {
			resolution.State = "unsupported"
		} else if s.Archive != nil {
			resolution, err = s.Archive.Resolve(ctx, entry.Reference)
			if err != nil {
				return Context{}, err
			}
			resolution.Evidence.ID, resolution.Evidence.Passage = entry.ID, entry.Passage
		}
		if entry.Pending != nil && bounded(entry.Pending.Quote, MaxChars) {
			resolution.SavedQuote = entry.Pending.Quote
		}
		if digest(entry.Passage) {
			for _, quote := range quotes {
				matches := PassageID(entry.Reference, quote) == entry.Passage
				if len(entry.legacy) > 0 {
					matches = legacyPassageID(entry.legacy, quote) == entry.Passage
				}
				if matches {
					resolution.SavedQuote = quote
					break
				}
			}
		}
		// A delivered msgvault entry has its own passage identity; available text still supplies the saved words.
		if resolution.SavedQuote == "" && resolution.State == "available" {
			resolution.SavedQuote = resolution.Evidence.Excerpt
		}
		if len(entry.legacy) > 0 {
			// Return a normalized Docbank reference with matching Docbank identities.
			resolution.Evidence.ID = ID(entry.Reference)
			resolution.Evidence.Passage = PassageID(entry.Reference, resolution.SavedQuote)
		}
		result.Passages = append(result.Passages, resolution)
	}
	if end < len(entries) {
		result.NextOffset = end
	}
	return result, nil
}

func legacyPassageID(raw jsontext.Value, quote string) string {
	var entry struct {
		Reference struct {
			Kind             string             `json:"kind"`
			ArchiveUID       string             `json:"archive_uid"`
			MessageID        int64              `json:"message_id"`
			SourceType       string             `json:"source_type"`
			SourceIdentifier string             `json:"source_identifier"`
			SourceMessageID  string             `json:"source_message_id"`
			OccurrenceKey    string             `json:"occurrence_key"`
			Docbank          *DocumentReference `json:"docbank_rendition"`
		} `json:"reference"`
	}
	if json.Unmarshal(raw, &entry) != nil || entry.Reference.Docbank == nil {
		return ""
	}
	r := entry.Reference
	location := Digest("msgvault.kata.passage-location.v1", r.Kind, r.ArchiveUID, strconv.FormatInt(r.MessageID, 10), r.SourceType, r.SourceIdentifier, r.SourceMessageID, r.OccurrenceKey, "", strconv.Itoa(r.Docbank.StartRune), strconv.Itoa(r.Docbank.EndRune))
	return Digest("msgvault.kata.passage.v1", location, quote)
}

func savedQuotes(body string) []string {
	lines := strings.Split(body, "\n")
	var result []string
	for i := 0; i+2 < len(lines); i++ {
		if !strings.HasPrefix(lines[i], "Evidence: ") || lines[i+1] != "" || !strings.HasPrefix(lines[i+2], "> ") {
			continue
		}
		i += 2
		var words []string
		for i < len(lines) && strings.HasPrefix(lines[i], "> ") {
			words = append(words, strings.TrimPrefix(lines[i], "> "))
			i++
		}
		result = append(result, strings.Join(words, "\n"))
		i--
	}
	return result
}
