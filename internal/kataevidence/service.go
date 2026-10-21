package kataevidence

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/kataclient"
)

type Kata interface {
	CheckProject(ref string) error
	Get(ctx context.Context, ref string) (kataclient.Issue, error)
	Create(ctx context.Context, key, title, body string, metadata map[string]jsontext.Value) (kataclient.Issue, error)
	FindMarker(ctx context.Context, marker string) (*kataclient.Issue, error)
	Patch(ctx context.Context, ref, key string, previous, value jsontext.Value) (kataclient.Issue, error)
	GetComments(ctx context.Context, ref string) ([]string, error)
	Comment(ctx context.Context, ref, key, body string) error
}

type Service struct {
	Kata    Kata
	Archive Archive
	Now     func() time.Time
}

// Entry retains the selected words; Pending marks an undelivered comment.
type Entry struct {
	ID        string    `json:"id"`
	Reference Reference `json:"reference"`
	Quote     string    `json:"quote"`
	Label     string    `json:"label"`
	Pending   *Pending  `json:"pending,omitzero"`
}
type Pending struct {
	SavedAt time.Time `json:"saved_at"`
}

// envelope retains unknown entries and fields verbatim, including future source kinds.
type envelope struct {
	fields  map[string]jsontext.Value
	entries []jsontext.Value
}

func parseEnvelope(data jsontext.Value) (envelope, error) {
	e := envelope{fields: map[string]jsontext.Value{"version": jsontext.Value("1")}}
	if len(data) == 0 {
		return e, nil
	}
	if len(data) > 512<<10 || json.Unmarshal(data, &e.fields) != nil {
		return e, ErrUnsupported
	}
	var version int
	if json.Unmarshal(e.fields["version"], &version) != nil || version != 1 {
		return e, ErrUnsupported
	}
	if json.Unmarshal(e.fields["entries"], &e.entries) != nil || len(e.entries) > MaxEntries {
		return e, ErrUnsupported
	}
	return e, nil
}
func (e envelope) encode() (jsontext.Value, error) {
	entries, err := json.Marshal(e.entries)
	if err != nil {
		return nil, err
	}
	e.fields["entries"] = entries
	encoded, err := json.Marshal(e.fields)
	if err != nil {
		return nil, err
	}
	if len(encoded) > 512<<10 {
		return nil, ErrUnsupported
	}
	return encoded, nil
}

func parseNative(data jsontext.Value) (Entry, bool) {
	var e Entry
	if json.Unmarshal(data, &e) != nil || ValidateReference(e.Reference) != nil ||
		e.ID != ID(e.Reference) {
		return e, false
	}
	if !bounded(e.Quote, MaxChars) || !bounded(e.Label, 512) {
		return e, false
	}
	return e, true
}

func bounded(text string, limit int) bool {
	return utf8.ValidString(text) && utf8.RuneCountInString(text) <= limit
}

func validateCiteRequest(r CiteRequest) error {
	p := r.Selector
	validNode := p.NodeID > 0 && p.Path == "" || p.NodeID == 0 && strings.HasPrefix(p.Path, "/") && len(p.Path) <= 4096 && utf8.ValidString(p.Path) && !strings.ContainsRune(p.Path, 0)
	if !validNode {
		return fmt.Errorf("%w: selector requires a node ID or absolute path", ErrInvalid)
	}
	if p.StartRune < 0 {
		return fmt.Errorf("%w: --start must be nonnegative", ErrInvalid)
	}
	if p.EndRune <= p.StartRune || p.EndRune > 2147483647 || p.EndRune-p.StartRune > MaxChars {
		return fmt.Errorf("%w: --end must follow --start by at most 1000 code points", ErrInvalid)
	}
	if !bounded(p.Quote, MaxChars) {
		return fmt.Errorf("%w: --quote must contain at most 1000 UTF-8 code points", ErrInvalid)
	}
	if p.ExpectedVersion != "" && canonical.ValidateUUIDv4(p.ExpectedVersion) != nil {
		return fmt.Errorf("%w: --expected-version requires a content version UUID", ErrInvalid)
	}
	if !bounded(r.Brief, 2000) {
		return fmt.Errorf("%w: --brief exceeds 2000 code points", ErrInvalid)
	}
	if !bounded(r.Title, 512) {
		return fmt.Errorf("%w: --title exceeds 512 code points", ErrInvalid)
	}
	if len(r.Issue) > 256 {
		return fmt.Errorf("%w: --issue exceeds 256 bytes", ErrInvalid)
	}
	// ResolveTextCitation owns citation validation. Here we bind the selector to it.
	if p.Citation != nil && documentCitationMismatch(p) {
		return fmt.Errorf("%w: --citation does not match the selected node or range", ErrInvalid)
	}
	if r.Issue != "" {
		if r.Title != "" || r.Brief != "" || r.IdempotencyKey != "" {
			return fmt.Errorf("%w: --issue cannot be combined with --title, --brief, or --idempotency-key", ErrInvalid)
		}
	} else {
		if strings.TrimSpace(r.Title) == "" {
			return fmt.Errorf("%w: --title is required", ErrInvalid)
		}
		if strings.TrimSpace(r.IdempotencyKey) == "" || !utf8.ValidString(r.IdempotencyKey) || len(r.IdempotencyKey) > 128 || strings.ContainsAny(r.IdempotencyKey, "\r\n\x00") {
			return fmt.Errorf("%w: --idempotency-key requires 1 to 128 bytes without line breaks", ErrInvalid)
		}
	}
	return nil
}

func documentCitationMismatch(p Selector) bool {
	return p.NodeID != 0 && p.Citation.NodeID != p.NodeID || p.Citation.Start != p.StartRune || p.Citation.End != p.EndRune
}

func (s Service) Cite(ctx context.Context, r CiteRequest) (kataclient.Issue, error) {
	if err := validateCiteRequest(r); err != nil {
		return kataclient.Issue{}, err
	}
	if s.Archive == nil {
		return kataclient.Issue{}, ErrUnavailable
	}
	if r.Issue != "" {
		return s.append(ctx, r)
	}
	// Persist a request marker in addition to Kata's replay window.
	requestHash := requestDigest(r)
	// Lookup retries before reading source bytes: a filed issue survives source deletion.
	marker := createMarker(s.Archive.VaultUID(), r.IdempotencyKey)
	existing, err := s.Kata.FindMarker(ctx, marker)
	if err != nil {
		return kataclient.Issue{}, err
	}
	if existing != nil {
		if err := s.Kata.CheckProject(existing.QualifiedRef); err != nil {
			return kataclient.Issue{}, err
		}
		var recorded string
		if json.Unmarshal(existing.Metadata["docbank.request"], &recorded) != nil || recorded != requestHash {
			return kataclient.Issue{}, ErrIdempotencyConflict
		}
		return *existing, nil
	}
	evidence, err := s.Archive.Prepare(ctx, r.Selector)
	if err != nil {
		return kataclient.Issue{}, err
	}
	entry, _ := json.Marshal(Entry{ID: evidence.ID, Reference: evidence.Reference, Quote: evidence.Excerpt, Label: evidence.Label})
	env := envelope{fields: map[string]jsontext.Value{"version": jsontext.Value("1")}, entries: []jsontext.Value{entry}}
	data, err := env.encode()
	if err != nil {
		return kataclient.Issue{}, err
	}
	markerJSON, _ := json.Marshal(marker)
	requestJSON, _ := json.Marshal(requestHash)
	body := renderQuote(evidence.Label, evidence.Excerpt)
	if r.Brief != "" {
		body = r.Brief + "\n\n" + body
	}
	return s.Kata.Create(ctx, marker, r.Title, body, map[string]jsontext.Value{MetadataKey: data, "docbank.action": markerJSON, "docbank.request": requestJSON})
}

func (s Service) append(ctx context.Context, r CiteRequest) (kataclient.Issue, error) {
	if err := s.Kata.CheckProject(r.Issue); err != nil {
		return kataclient.Issue{}, err
	}

	// A pinned selection already recorded on the issue is a retry, even if its source is gone.
	current, err := s.Kata.Get(ctx, r.Issue)
	if err != nil {
		return kataclient.Issue{}, err
	}
	existing, err := parseEnvelope(current.Metadata[MetadataKey])
	if err != nil {
		return kataclient.Issue{}, err
	}
	for _, raw := range existing.entries {
		entry, valid := parseNative(raw)
		if valid && matchesSelection(entry.Reference, r.Selector, s.Archive.VaultUID()) &&
			(r.Selector.Quote == "" || entry.Quote == r.Selector.Quote) {
			return s.finishPending(ctx, r.Issue, current, entry.ID)
		}
	}
	evidence, err := s.Archive.Prepare(ctx, r.Selector)
	if err != nil {
		return kataclient.Issue{}, err
	}
	for range 3 {
		issue, err := s.Kata.Get(ctx, r.Issue)
		if err != nil {
			return kataclient.Issue{}, err
		}
		env, err := parseEnvelope(issue.Metadata[MetadataKey])
		if err != nil {
			return kataclient.Issue{}, err
		}
		found := false
		for _, raw := range env.entries {
			entry, valid := parseNative(raw)
			if valid && entry.ID == evidence.ID {
				found = true
			}
		}
		if !found {
			if len(env.entries) >= MaxEntries {
				return kataclient.Issue{}, ErrEntryLimit
			}
			entry, _ := json.Marshal(Entry{ID: evidence.ID, Reference: evidence.Reference, Quote: evidence.Excerpt, Label: evidence.Label, Pending: &Pending{}})
			env.entries = append(env.entries, entry)
			data, err := env.encode()
			if err != nil {
				return kataclient.Issue{}, err
			}
			issue, err = s.Kata.Patch(ctx, r.Issue, MetadataKey, issue.Metadata[MetadataKey], data)
			if kataclient.IsGuardConflict(err) {
				continue
			}
			if err != nil {
				return kataclient.Issue{}, err
			}
		}
		return s.finishPending(ctx, r.Issue, issue, evidence.ID)
	}
	return kataclient.Issue{}, ErrConflict
}

func matchesSelection(ref Reference, p Selector, vault string) bool {
	expectedVersion := p.ExpectedVersion
	if expectedVersion == "" && p.Citation != nil {
		expectedVersion = p.Citation.ContentVersionID.String()
	}
	if expectedVersion == "" {
		return false
	}
	nodeID := p.NodeID
	if nodeID == 0 && p.Citation != nil {
		nodeID = p.Citation.NodeID
	}
	if c := ref.Rendition; c != nil {
		return p.Citation != nil && c.VaultUID.String() == vault && c.NodeID == nodeID &&
			c.ContentVersionID.String() == expectedVersion && *c == *p.Citation
	}
	d := ref.Docbank
	return p.Citation == nil && d.VaultUID == vault && d.NodeID == nodeID &&
		d.ContentVersionID == expectedVersion && d.StartRune == p.StartRune && d.EndRune == p.EndRune
}

func (s Service) finishPending(ctx context.Context, ref string, issue kataclient.Issue, entryID string) (kataclient.Issue, error) {
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	var comments []string
	commentsRead := false
	for range 3 {
		env, err := parseEnvelope(issue.Metadata[MetadataKey])
		if err != nil {
			return kataclient.Issue{}, err
		}
		unstamped := false
		unstampedEntries := make(map[string]bool)
		for i, raw := range env.entries {
			entry, valid := parseNative(raw)
			if valid && entry.ID == entryID && entry.Pending != nil && entry.Pending.SavedAt.IsZero() {
				var fields map[string]jsontext.Value
				if json.Unmarshal(raw, &fields) != nil {
					return kataclient.Issue{}, ErrUnsupported
				}
				fields["pending"], _ = json.Marshal(Pending{SavedAt: issue.UpdatedAt})
				env.entries[i], _ = json.Marshal(fields)
				unstamped = true
				unstampedEntries[entry.ID] = true
			}
		}
		if unstamped {
			data, err := env.encode()
			if err != nil {
				return kataclient.Issue{}, err
			}
			updated, err := s.Kata.Patch(ctx, ref, MetadataKey, issue.Metadata[MetadataKey], data)
			if kataclient.IsGuardConflict(err) {
				issue, err = s.Kata.Get(ctx, ref)
				if err != nil {
					return kataclient.Issue{}, err
				}
				continue
			}
			if err != nil {
				return kataclient.Issue{}, err
			}
			issue = updated
		}
		changed := false
		for i, raw := range env.entries {
			entry, valid := parseNative(raw)
			if !valid || entry.ID != entryID || entry.Pending == nil {
				continue
			}
			marker := "<!-- docbank-evidence:" + entry.ID + " -->"
			body := renderQuote(entry.Label, entry.Quote) + "\n" + marker + "\n"
			key := commentKey(issue.UID, entry.ID, body)
			posted := false
			// Leave a day of margin inside Kata's seven-day replay window.
			if !unstampedEntries[entry.ID] && !entry.Pending.SavedAt.IsZero() && now().Sub(entry.Pending.SavedAt) >= 6*24*time.Hour {
				if !commentsRead {
					comments, err = s.Kata.GetComments(ctx, ref)
					if failure, ok := errors.AsType[*kataclient.Error](err); ok && failure.Code == "response_too_large" {
						return kataclient.Issue{}, fmt.Errorf("stale pending quote can't be verified against the discussion: %w", ErrUnsupported)
					}
					if err != nil {
						return kataclient.Issue{}, err
					}
					commentsRead = true
				}
				posted = slices.ContainsFunc(comments, func(comment string) bool {
					return slices.Contains(strings.Split(comment, "\n"), marker)
				})
			}
			if !posted {
				if err := s.Kata.Comment(ctx, ref, key, body); err != nil {
					return kataclient.Issue{}, err
				}
			}
			// Clear only our field; preserve any future fields on the entry.
			var fields map[string]jsontext.Value
			if json.Unmarshal(raw, &fields) != nil {
				return kataclient.Issue{}, ErrUnsupported
			}
			delete(fields, "pending")
			env.entries[i], err = json.Marshal(fields)
			if err != nil {
				return kataclient.Issue{}, err
			}
			changed = true
		}
		if !changed {
			return issue, nil
		}
		data, err := env.encode()
		if err != nil {
			return kataclient.Issue{}, err
		}
		updated, err := s.Kata.Patch(ctx, ref, MetadataKey, issue.Metadata[MetadataKey], data)
		if !kataclient.IsGuardConflict(err) {
			return updated, err
		}
		issue, err = s.Kata.Get(ctx, ref)
		if err != nil {
			return kataclient.Issue{}, err
		}
	}
	return kataclient.Issue{}, ErrConflict
}

func renderQuote(label, words string) string {
	words = strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(words)
	return "Evidence: " + strings.NewReplacer("\n", " ", "\r", " ").Replace(label) + "\n\n> " + strings.ReplaceAll(words, "\n", "\n> ") + "\n"
}

func requestDigest(request CiteRequest) string {
	encoded, _ := json.Marshal(request)
	return Digest("docbank.kata.request.v1", string(encoded))
}

func createMarker(vault, key string) string { return Digest("docbank.kata.create.v1", vault, key) }

func commentKey(uid, entryID, body string) string {
	return Digest("docbank.kata.comment.v1", uid, entryID, body)
}
