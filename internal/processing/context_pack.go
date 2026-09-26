package processing

import (
	"encoding/json/v2"
	"errors"
	"slices"
	"sort"

	"go.kenn.io/docbank/document"
)

const (
	DefaultContextPackBytes      = 64 << 10
	MaxContextPackBytes          = 256 << 10
	DefaultContextPackDocuments  = 20
	DefaultContextPassagesPerDoc = 2
)

var ErrContextBudget = errors.New("context pack budget cannot hold its metadata")

type ContextCoverage struct {
	RequestedSources          int `json:"requested_sources"`
	AvailableSources          int `json:"available_sources"`
	SelectedSources           int `json:"selected_sources"`
	RenditionAvailableSources int `json:"rendition_available_sources"`
	RenditionMissingSources   int `json:"rendition_missing_sources"`
}

type ContextPassage struct {
	Ref     document.PassageRefV1 `json:"ref"`
	Text    string                `json:"text"`
	Path    string                `json:"path"`
	Reasons []string              `json:"reasons"`
}

type ContextPack struct {
	FenceFingerprint    string           `json:"fence_fingerprint"`
	ProfileFingerprint  string           `json:"profile_fingerprint,omitempty"`
	IndexGenerationID   string           `json:"index_generation_id,omitempty"`
	IndexManifestDigest string           `json:"index_manifest_digest,omitempty"`
	Coverage            ContextCoverage  `json:"coverage"`
	Passages            []ContextPassage `json:"passages"`
	Omitted             map[string]int   `json:"omitted"`
	Deduplicated        int              `json:"deduplicated"`
	SearchTruncated     bool             `json:"search_truncated"`
	Complete            bool             `json:"complete"`
	Truncated           bool             `json:"truncated"`
}

type contextCandidate struct {
	Ref     document.PassageRefV1
	Body    []byte
	Path    string
	Reasons []string
}

func assembleContextPack(fingerprint string, requested int, candidates []contextCandidate,
	maxBytes, perDocument, maxDocuments int,
) (ContextPack, error) {
	merged, deduplicated, err := mergeContextCandidates(candidates)
	if err != nil {
		return ContextPack{}, err
	}
	available := make(map[string]struct{}, len(merged))
	for _, candidate := range merged {
		available[candidate.Ref.DocumentUID] = struct{}{}
	}
	pack := ContextPack{FenceFingerprint: fingerprint,
		Coverage: ContextCoverage{RequestedSources: requested, AvailableSources: len(available)},
		Passages: []ContextPassage{}, Omitted: map[string]int{}, Deduplicated: deduplicated,
		Complete: true}
	if !contextPackFits(pack, maxBytes) {
		return ContextPack{}, ErrContextBudget
	}
	selected := make(map[string]int, maxDocuments)
	for _, candidate := range merged {
		uid := candidate.Ref.DocumentUID
		if selected[uid] >= perDocument {
			pack.Omitted["per_document_limit"]++
			continue
		}
		if selected[uid] == 0 && len(selected) >= maxDocuments {
			pack.Omitted["document_limit"]++
			continue
		}
		passage := ContextPassage{Ref: candidate.Ref,
			Text: string(candidate.Body[candidate.Ref.ByteStart:candidate.Ref.ByteEnd]),
			Path: candidate.Path, Reasons: append([]string(nil), candidate.Reasons...)}
		pack.Passages = append(pack.Passages, passage)
		if selected[uid] == 0 {
			pack.Coverage.SelectedSources++
		}
		if !contextPackFits(pack, maxBytes) {
			pack.Passages = pack.Passages[:len(pack.Passages)-1]
			if selected[uid] == 0 {
				pack.Coverage.SelectedSources--
			}
			pack.Omitted["byte_budget"]++
			continue
		}
		selected[uid]++
	}
	return finishContextPackBudget(pack, maxBytes)
}

// finishContextPackBudget charges all metadata attached after candidate
// selection, including omission counts and generation/profile receipts.
func finishContextPackBudget(pack ContextPack, maxBytes int) (ContextPack, error) {
	pack.Complete = len(pack.Omitted) == 0 && !pack.SearchTruncated
	pack.Truncated = !pack.Complete
	selected := make(map[string]int, len(pack.Passages))
	for _, passage := range pack.Passages {
		selected[passage.Ref.DocumentUID]++
	}
	// Omission counts and booleans are part of the wire budget too. Remove the
	// lowest-priority selected passage until the complete envelope fits.
	for !contextPackFits(pack, maxBytes) && len(pack.Passages) != 0 {
		last := pack.Passages[len(pack.Passages)-1]
		pack.Passages = pack.Passages[:len(pack.Passages)-1]
		selected[last.Ref.DocumentUID]--
		if selected[last.Ref.DocumentUID] == 0 {
			delete(selected, last.Ref.DocumentUID)
			pack.Coverage.SelectedSources--
		}
		pack.Omitted["byte_budget"]++
		pack.Complete, pack.Truncated = false, true
	}
	if !contextPackFits(pack, maxBytes) {
		return ContextPack{}, ErrContextBudget
	}
	return pack, nil
}

func contextPackFits(pack ContextPack, maxBytes int) bool {
	if maxBytes < 1 || maxBytes > MaxContextPackBytes {
		return false
	}
	encoded, err := json.Marshal(pack)
	return err == nil && len(encoded) <= maxBytes
}

func mergeContextCandidates(candidates []contextCandidate) ([]contextCandidate, int, error) {
	merged := make([]contextCandidate, 0, len(candidates))
	deduplicated := 0
	for _, candidate := range candidates {
		if err := document.ValidatePassageRefV1(candidate.Ref, candidate.Body); err != nil {
			return nil, 0, ErrPassageCorrupt
		}
		candidate.Reasons = sortedUniqueContextReasons(candidate.Reasons)
		insertAt := len(merged)
		for {
			changed := false
			for index := 0; index < len(merged); {
				prior := merged[index]
				if !overlappingContextCandidate(prior, candidate) {
					index++
					continue
				}
				insertAt = min(insertAt, index)
				start := min(prior.Ref.ByteStart, candidate.Ref.ByteStart)
				end := max(prior.Ref.ByteEnd, candidate.Ref.ByteEnd)
				ref, err := document.NewPassageRefV1(candidate.Ref, candidate.Body, start, end)
				if err != nil {
					return nil, 0, ErrPassageCorrupt
				}
				candidate.Ref = ref
				candidate.Reasons = sortedUniqueContextReasons(append(candidate.Reasons, prior.Reasons...))
				merged = append(merged[:index], merged[index+1:]...)
				deduplicated++
				changed = true
			}
			if !changed {
				break
			}
		}
		if insertAt >= len(merged) {
			merged = append(merged, candidate)
		} else {
			merged = slices.Insert(merged, insertAt, candidate)
		}
	}
	return merged, deduplicated, nil
}

func overlappingContextCandidate(left, right contextCandidate) bool {
	return left.Ref.DocumentUID == right.Ref.DocumentUID &&
		left.Ref.ContentVersionID == right.Ref.ContentVersionID &&
		left.Ref.RenditionBuildID == right.Ref.RenditionBuildID &&
		left.Ref.AttachmentID == right.Ref.AttachmentID &&
		left.Ref.BodySHA256 == right.Ref.BodySHA256 &&
		left.Ref.ByteStart < right.Ref.ByteEnd && right.Ref.ByteStart < left.Ref.ByteEnd
}

func sortedUniqueContextReasons(reasons []string) []string {
	result := slices.Clone(reasons)
	sort.Strings(result)
	return slices.Compact(result)
}
