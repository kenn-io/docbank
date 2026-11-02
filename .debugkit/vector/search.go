package vector

import (
	"errors"
	"math"
	"sort"
)

// Hit is a single search result identifying the document it belongs to. K
// is the caller's document key type (for example int64 or a UUID); this
// package compares keys for equality but never interprets them.
type Hit[K comparable] struct {
	// Doc identifies the source document.
	Doc K
	// ChunkIndex is the chunk within Doc that matched.
	ChunkIndex int
	// Revision identifies the indexed content that produced Score. Backends
	// without revision tracking leave it nil. Hydration must not attach this
	// score to a different revision of the document.
	Revision any
	// Score is the backend's similarity score for this chunk. Merge
	// overwrites it with the merged score under the chosen strategy.
	Score float32
}

// RollupByDocument reduces chunk-level hits to one hit per document,
// keeping the highest-scoring chunk for each, and returns them sorted by
// score descending. It is the chunk->document step a caller applies to a
// single generation's results before merging across generations. A NaN
// document key or score is an error and is not stored.
func RollupByDocument[K comparable](hits []Hit[K]) ([]Hit[K], error) {
	if len(hits) == 0 {
		return nil, nil
	}
	for _, h := range hits {
		if err := rejectStoredNaN(h.Doc, h.Score); err != nil {
			return nil, err
		}
	}
	best := make(map[K]Hit[K], len(hits))
	order := make([]K, 0, len(hits))
	for _, h := range hits {
		cur, ok := best[h.Doc]
		if !ok {
			order = append(order, h.Doc)
			best[h.Doc] = h
			continue
		}
		if h.Score > cur.Score {
			best[h.Doc] = h
		}
	}
	out := make([]Hit[K], 0, len(order))
	for _, k := range order {
		out = append(out, best[k])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out, nil
}

// MergeStrategy selects how Merge orders documents drawn from different
// generations, whose raw scores are not directly comparable.
type MergeStrategy int

const (
	// MergeNormalizedScore min-max normalizes each generation's scores to
	// [0,1] before ordering. It is the default: it keeps score signal
	// without letting one generation's score scale dominate.
	MergeNormalizedScore MergeStrategy = iota
	// MergeRawScore orders by raw score. Use it only when the generations
	// share a model family and comparable score distributions.
	MergeRawScore
	// MergeReciprocalRank ignores absolute scores and fuses by rank. Use
	// it when score distributions differ sharply between generations.
	MergeReciprocalRank
)

// MergeOptions configures Merge.
type MergeOptions struct {
	// Strategy selects the ordering policy. The zero value is
	// MergeNormalizedScore.
	Strategy MergeStrategy
	// RankConstant is the k term in reciprocal-rank fusion. Finite values
	// <= 0 use 60. NaN and either infinity are errors.
	RankConstant float64
	// Limit caps the number of returned hits. Values <= 0 return all.
	Limit int
}

// Merge unions per-generation, document-level result lists into one
// ranking. The lists are given in descending preference: when a document
// appears in more than one list, the hit from the earliest list is kept,
// which is how a caller expresses "prefer the newer generation" during a
// migration. Coverage is a union, so a document present in only one
// generation is never dropped.
//
// Each surviving hit's Score is set to the merged score under the chosen
// strategy, and the result is ordered by that score descending. A NaN
// document key, score, or reciprocal-rank constant is an error and is not
// stored.
func Merge[K comparable](perGeneration [][]Hit[K], o MergeOptions) ([]Hit[K], error) {
	for _, list := range perGeneration {
		for _, h := range list {
			if err := rejectStoredNaN(h.Doc, h.Score); err != nil {
				return nil, err
			}
		}
	}
	if o.Strategy == MergeReciprocalRank && !isFinite(o.RankConstant) {
		return nil, ErrNonFinite
	}
	rep := make(map[K]Hit[K])
	order := make([]K, 0)
	score := make(map[K]float64)

	switch o.Strategy {
	case MergeReciprocalRank:
		k := o.RankConstant
		if k <= 0 {
			k = 60
		}
		for _, list := range perGeneration {
			for rank, h := range list {
				term := 1.0 / (k + float64(rank) + 1.0)
				if err := storeScore(score, h.Doc, score[h.Doc]+term); err != nil {
					return nil, err
				}
				if _, ok := rep[h.Doc]; !ok {
					rep[h.Doc] = h
					order = append(order, h.Doc)
				}
			}
		}
	case MergeRawScore:
		for _, list := range perGeneration {
			for _, h := range list {
				if _, ok := rep[h.Doc]; ok {
					continue
				}
				if err := storeScore(score, h.Doc, float64(h.Score)); err != nil {
					return nil, err
				}
				rep[h.Doc] = h
				order = append(order, h.Doc)
			}
		}
	default: // MergeNormalizedScore
		for _, list := range perGeneration {
			lo, hi := scoreRange(list)
			// Subtract in float64. MaxFloat32 minus its negation overflows
			// float32 to +Inf, and Inf/Inf is NaN.
			span := float64(hi) - float64(lo)
			for _, h := range list {
				if _, ok := rep[h.Doc]; ok {
					continue
				}
				normalized := 1.0
				if span > 0 {
					normalized = (float64(h.Score) - float64(lo)) / span
				}
				if err := storeScore(score, h.Doc, normalized); err != nil {
					return nil, err
				}
				rep[h.Doc] = h
				order = append(order, h.Doc)
			}
		}
	}

	out := make([]Hit[K], 0, len(order))
	for _, doc := range order {
		h := rep[doc]
		stored := float32(score[doc])
		if !isFinite(float64(stored)) {
			return nil, ErrNonFinite
		}
		h.Score = stored
		out = append(out, h)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if o.Limit > 0 && len(out) > o.Limit {
		out = out[:o.Limit]
	}
	return out, nil
}

// ErrNonFinite reports a NaN or infinite score, or a key that is not equal to
// itself, in search results. A store returned a distance or score that
// cannot be ranked.
var ErrNonFinite = errors.New("vector: non-finite score or key")

// rejectStoredNaN reports a document key or score that cannot be put in a
// result map. NaN is not equal to itself, and neither is a key that contains
// NaN. An infinite score is rejected too, because later arithmetic turns
// infinities into NaN.
func rejectStoredNaN[K comparable](key K, score float32) error {
	other := key
	if key != other || !isFinite(float64(score)) {
		return ErrNonFinite
	}
	return nil
}

func isFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

func storeScore[K comparable](scores map[K]float64, key K, value float64) error {
	if !isFinite(value) {
		return ErrNonFinite
	}
	scores[key] = value
	return nil
}

func scoreRange[K comparable](hits []Hit[K]) (lo, hi float32) {
	for i, h := range hits {
		if i == 0 || h.Score < lo {
			lo = h.Score
		}
		if i == 0 || h.Score > hi {
			hi = h.Score
		}
	}
	return lo, hi
}
