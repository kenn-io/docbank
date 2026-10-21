package kataevidence

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
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
	Comment(ctx context.Context, ref, key, body string) error
}

type Service struct {
	Kata    Kata
	Archive Archive
}

// Entry and Pending mirror msgvault's version-1 evidence envelope.
type Entry struct {
	ID        string    `json:"id"`
	Passage   string    `json:"passage"`
	Reference Reference `json:"reference"`
	Pending   *Pending  `json:"pending,omitzero"`
}
type Pending struct {
	Quote string `json:"quote"`
	Label string `json:"label"`
}

// envelope retains unknown entries and fields verbatim, including future source kinds.
type envelope struct {
	fields  map[string]jsontext.Value
	entries []jsontext.Value
}

func parseEnvelope(data jsontext.Value) (envelope, error) {
	return parseEnvelopeBound(data, MaxEntries, 512<<10)
}

func parseEnvelopeBound(data jsontext.Value, limit, maxBytes int) (envelope, error) {
	e := envelope{fields: map[string]jsontext.Value{"version": jsontext.Value("1")}}
	if len(data) == 0 {
		return e, nil
	}
	if len(data) > maxBytes || json.Unmarshal(data, &e.fields) != nil {
		return e, ErrUnsupported
	}
	var version int
	if json.Unmarshal(e.fields["version"], &version) != nil || version != 1 {
		return e, ErrUnsupported
	}
	if json.Unmarshal(e.fields["entries"], &e.entries) != nil || len(e.entries) > limit {
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
		e.ID != ID(e.Reference) || !canonical.IsSHA256Hex(e.Passage) {
		return e, false
	}
	if e.Pending != nil && (!bounded(e.Pending.Quote, MaxChars) || !bounded(e.Pending.Label, 512) || PassageID(e.Reference, e.Pending.Quote) != e.Passage) {
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
	if !validNode || p.StartRune < 0 || p.EndRune <= p.StartRune || p.EndRune > 2147483647 || p.EndRune-p.StartRune > MaxChars ||
		!bounded(p.Quote, MaxChars) || p.ExpectedVersion != "" && canonical.ValidateUUIDv4(p.ExpectedVersion) != nil ||
		!bounded(r.Brief, 2000) || !bounded(r.Title, 512) || len(r.Issue) > 256 {
		return ErrInvalid
	}
	if p.Citation != nil && (documentCitationMismatch(p) || documentValidate(p) != nil) {
		return ErrInvalid
	}
	if r.Issue != "" {
		if r.Title != "" || r.Brief != "" || r.IdempotencyKey != "" {
			return ErrInvalid
		}
	} else if strings.TrimSpace(r.Title) == "" || strings.TrimSpace(r.IdempotencyKey) == "" || !utf8.ValidString(r.IdempotencyKey) || len(r.IdempotencyKey) > 128 || strings.ContainsAny(r.IdempotencyKey, "\r\n\x00") {
		return ErrInvalid
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
	encoded, _ := json.Marshal(r)
	requestHash := Digest("docbank.kata.request.v1", string(encoded))
	// Lookup retries before reading source bytes: a filed issue survives source deletion.
	marker := Digest("docbank.kata.create.v1", s.Archive.VaultUID(), r.IdempotencyKey)
	existing, err := s.Kata.FindMarker(ctx, marker)
	if err != nil {
		return kataclient.Issue{}, err
	}
	if existing != nil {
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
	if err = validateEvidence(evidence); err != nil {
		return kataclient.Issue{}, err
	}
	entry, _ := json.Marshal(Entry{ID: evidence.ID, Passage: evidence.Passage, Reference: evidence.Reference})
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

func validateEvidence(e Evidence) error {
	if ValidateReference(e.Reference) != nil || e.ID != ID(e.Reference) || e.Passage != PassageID(e.Reference, e.Excerpt) ||
		!bounded(e.Label, 512) || !bounded(e.Excerpt, MaxChars) || utf8.RuneCountInString(e.Excerpt) != e.Reference.Docbank.EndRune-e.Reference.Docbank.StartRune {
		return ErrInvalid
	}
	return nil
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
			(r.Selector.Quote == "" || PassageID(entry.Reference, r.Selector.Quote) == entry.Passage) {
			return s.finishPending(ctx, r.Issue, current)
		}
	}
	evidence, err := s.Archive.Prepare(ctx, r.Selector)
	if err != nil {
		return kataclient.Issue{}, err
	}
	if err = validateEvidence(evidence); err != nil {
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
				return kataclient.Issue{}, fmt.Errorf("Kata issue already has %d Docbank evidence entries: %w", MaxEntries, ErrUnsupported)
			}
			entry, _ := json.Marshal(Entry{ID: evidence.ID, Passage: evidence.Passage, Reference: evidence.Reference, Pending: &Pending{Quote: evidence.Excerpt, Label: evidence.Label}})
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
		return s.finishPending(ctx, r.Issue, issue)
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
	d := ref.Docbank
	nodeID := p.NodeID
	if nodeID == 0 && p.Citation != nil {
		nodeID = p.Citation.NodeID
	}
	if d.VaultUID != vault || d.NodeID != nodeID || d.ContentVersionID != expectedVersion || d.StartRune != p.StartRune || d.EndRune != p.EndRune {
		return false
	}
	if p.Citation == nil {
		return d.RenditionAttachmentID == ""
	}
	citation, err := d.TextCitation()
	return err == nil && citation == *p.Citation
}

func (s Service) finishPending(ctx context.Context, ref string, issue kataclient.Issue) (kataclient.Issue, error) {
	for range 3 {
		env, err := parseEnvelope(issue.Metadata[MetadataKey])
		if err != nil {
			return kataclient.Issue{}, err
		}
		changed := false
		for i, raw := range env.entries {
			entry, valid := parseNative(raw)
			if !valid || entry.Pending == nil {
				continue
			}
			body := renderQuote(entry.Pending.Label, entry.Pending.Quote)
			key := Digest("docbank.kata.comment.v1", issue.UID, entry.Passage, body)
			if !slices.ContainsFunc(issue.Comments, func(comment kataclient.Comment) bool { return comment.Body == body }) {
				if err := s.Kata.Comment(ctx, ref, key, body); err != nil {
					return kataclient.Issue{}, err
				}
				issue.Comments = append(issue.Comments, kataclient.Comment{Body: body})
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
	return "Evidence: " + strings.NewReplacer("\n", " ", "\r", " ").Replace(label) + "\n\n> " + strings.ReplaceAll(words, "\n", "\n> ") + "\n"
}
