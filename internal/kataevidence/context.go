package kataevidence

import (
	"context"

	"go.kenn.io/docbank/internal/kataclient"
)

type Context struct {
	Issue      kataclient.Issue `json:"issue"`
	Passages   []Resolution     `json:"passages"`
	NextOffset int              `json:"next_offset,omitzero"`
}

// Context reads native evidence metadata and never starts processing.
func (s Service) Context(ctx context.Context, r ContextRequest) (Context, error) {
	if r.Offset < 0 || r.Offset > MaxContextOffset || r.Issue == "" {
		return Context{}, ErrInvalid
	}
	if err := s.Kata.CheckProject(r.Issue); err != nil {
		return Context{}, err
	}
	issue, err := s.Kata.Get(ctx, r.Issue)
	if err != nil {
		return Context{}, err
	}
	env, err := parseEnvelope(issue.Metadata[MetadataKey])
	if err != nil {
		return Context{}, err
	}
	result := Context{Issue: issue, Passages: []Resolution{}}
	start := min(r.Offset, len(env.entries))
	end := min(start+ContextPageSize, len(env.entries))
	for _, raw := range env.entries[start:end] {
		entry, valid := parseNative(raw)
		resolution := Resolution{State: "unsupported", Evidence: Evidence{ID: entry.ID, Reference: entry.Reference, ContentTrust: "untrusted"}}
		if valid {
			resolution.State = "unreachable"
			if s.Archive != nil {
				resolution, err = s.Archive.Resolve(ctx, entry.Reference, entry.Quote)
				if err != nil {
					return Context{}, err
				}
				resolution.Evidence.ID = entry.ID
			}
		}
		if bounded(entry.Quote, MaxChars) && bounded(entry.Label, 512) {
			resolution.SavedQuote = entry.Quote
			resolution.Evidence.Label = entry.Label
		}
		resolution.Pending = entry.Pending
		result.Passages = append(result.Passages, resolution)
	}
	if end < len(env.entries) {
		result.NextOffset = end
	}
	return result, nil
}
