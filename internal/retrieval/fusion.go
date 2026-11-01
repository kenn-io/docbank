package retrieval

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/docbank/document/embedding"
)

func FuseReciprocalRank(lexical, semantic []Candidate, limit int) ([]Result, bool, error) {
	if err := validateOneSemanticVectorSpace(semantic); err != nil {
		return nil, false, err
	}
	byDocument := make(map[fusionIdentity]*Result, len(lexical)+len(semantic))
	if err := addLane(byDocument, lexical, LaneLexical); err != nil {
		return nil, false, fmt.Errorf("lexical lane: %w", err)
	}
	if err := addLane(byDocument, semantic, LaneSemantic); err != nil {
		return nil, false, fmt.Errorf("semantic lane: %w", err)
	}
	identities := make([]fusionIdentity, 0, len(byDocument))
	for identity := range byDocument {
		identities = append(identities, identity)
	}
	slices.SortFunc(identities, func(leftKey, rightKey fusionIdentity) int {
		left, right := byDocument[leftKey].Document, byDocument[rightKey].Document
		return cmp.Or(cmp.Compare(left.VaultID, right.VaultID),
			cmp.Compare(left.NodeID, right.NodeID), cmp.Compare(left.ContentVersionID, right.ContentVersionID))
	})
	// Shared fusion orders string keys before truncation. Ordinals preserve
	// the document order, including numeric node IDs, without encoding evidence.
	keys := make(map[fusionIdentity]string, len(identities))
	metadata := make(map[string]*Result, len(identities))
	for index, identity := range identities {
		key := fmt.Sprintf("%020d", index)
		keys[identity], metadata[key] = key, byDocument[identity]
	}
	rankLane := func(candidates []Candidate) (lane embedding.ScopedCandidates) {
		for _, candidate := range candidates { // Copies of one displayed photo keep their best rank.
			key := keys[candidateFusionIdentity(candidate)]
			if !slices.ContainsFunc(lane.Candidates, func(kept embedding.RankedCandidate) bool { return kept.Key == key }) {
				lane.Candidates = append(lane.Candidates, embedding.RankedCandidate{Key: key, Rank: candidate.Rank, Score: candidate.Score})
			}
		}
		return lane
	}
	fused, err := embedding.FuseReciprocalRank(embedding.FusionInput{
		Lexical: rankLane(lexical), Semantic: rankLane(semantic),
	}, limit)
	if err != nil {
		return nil, false, err
	}
	results := make([]Result, len(fused.Candidates))
	for index, candidate := range fused.Candidates {
		result := *metadata[candidate.Key]
		result.Rank, result.Score = candidate.Rank, candidate.Score
		result.Explanation = fusionContributions(candidate)
		results[index] = result
	}
	return results, fused.Truncated, nil
}

func validateOneSemanticVectorSpace(candidates []Candidate) error {
	vectorSpaceID := ""
	for _, candidate := range candidates {
		if candidate.VectorSpaceID == "" {
			return errors.New("semantic candidates require one active vector space")
		}
		if vectorSpaceID == "" {
			vectorSpaceID = candidate.VectorSpaceID
		} else if candidate.VectorSpaceID != vectorSpaceID {
			return errors.New("semantic candidates must belong to one active vector space")
		}
		for _, evidence := range candidate.Evidence {
			if evidence.VectorSpaceID != candidate.VectorSpaceID {
				return errors.New("semantic evidence must belong to one active vector space")
			}
		}
	}
	return nil
}

func addLane(results map[fusionIdentity]*Result, candidates []Candidate, lane Lane) error {
	for _, candidate := range candidates {
		if candidate.Lane != lane {
			return errors.New("candidate belongs to the wrong lane")
		}
		if candidate.Document.VaultID == "" || candidate.Document.NodeID <= 0 || candidate.Document.ContentVersionID == "" {
			return errors.New("candidate document identity is incomplete")
		}
		key := candidateFusionIdentity(candidate)
		result := results[key]
		if result == nil {
			result = &Result{Document: candidate.Document, Path: candidate.Path}
			results[key] = result
		} else if lane == LaneLexical && result.LexicalRank != 0 || lane == LaneSemantic && result.SemanticRank != 0 {
			continue
		}
		result.Path = cmp.Or(result.Path, candidate.Path)
		addCandidateContribution(result, candidate, lane)
		result.Evidence = append(result.Evidence, candidate.Evidence...)
	}
	return nil
}

type fusionIdentity struct {
	DocumentIdentity

	photoBlobHash string
}

func candidateFusionIdentity(candidate Candidate) fusionIdentity {
	if candidate.photoBlobHash != "" {
		return fusionIdentity{VaultID: candidate.Document.VaultID, photoBlobHash: candidate.photoBlobHash}
	}
	return fusionIdentity{DocumentIdentity: candidate.Document}
}

func fusionContributions(candidate embedding.FusedCandidate) []Contribution {
	var result []Contribution
	for _, signal := range []struct {
		lane   Lane
		signal *embedding.CandidateSignal
	}{{LaneLexical, candidate.Lexical}, {LaneSemantic, candidate.Semantic}} {
		if signal.signal != nil {
			result = append(result, Contribution{Lane: signal.lane, Rank: signal.signal.Rank,
				Contribution: 1 / float64(ReciprocalRankK+signal.signal.Rank)})
		}
	}
	return result
}

func addCandidateContribution(result *Result, candidate Candidate, lane Lane) {
	if lane == LaneLexical {
		result.LexicalRank = cmp.Or(result.LexicalRank, candidate.Rank) // Lanes list copies best first.
		result.Excerpt = cmp.Or(result.Excerpt, candidate.Excerpt)
		result.rerankExcerpt = cmp.Or(result.rerankExcerpt, candidate.Excerpt)
	} else {
		result.SemanticRank = cmp.Or(result.SemanticRank, candidate.Rank)
		nameOnly := len(result.Evidence) == 1 && result.Evidence[0].Kind == "node_name"
		if result.rerankExcerpt == "" || nameOnly && strings.TrimSpace(candidate.Excerpt) != "" {
			result.rerankExcerpt = candidate.Excerpt
		}
	}
}
