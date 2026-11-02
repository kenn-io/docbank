package rrf

import (
	"errors"
	"fmt"
	"math"
	"slices"
)

// Contribution is one leg's evidence for a fused key or group.
type Contribution struct {
	Leg    string
	Rank   int
	Weight float64
	Term   float64
}

// Key is a fusion key or group identity: a string, an integer, or a 16-byte
// id such as a UUID. The set excludes interface types, whose dynamic value
// may not be comparable, and floats, whose NaN is not equal to itself.
type Key interface {
	~string | ~int | ~int8 | ~int16 | ~int32 | ~int64 |
		~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 | ~[16]byte
}

// Hit is a fused key. Contributions follow leg order.
type Hit[K Key] struct {
	Key           K
	Score         float64
	Contributions []Contribution
}

// Leg is one ranked list, best key first. A repeated key keeps its earliest
// rank and does not contribute twice.
type Leg[K Key] struct {
	Name   string
	Weight float64
	Keys   []K
}

// Fuse combines legs. ValidateLegs describes the rules for k, names and
// weights. The result is every key that appeared, highest score first. Fuse
// does not apply a result limit; callers limit after eligibility.
func Fuse[K Key](k float64, legs []Leg[K]) ([]Hit[K], error) {
	if err := ValidateLegs(k, len(legs), func(i int) (string, float64) {
		return legs[i].Name, legs[i].Weight
	}); err != nil {
		return nil, err
	}
	type state struct {
		hit   Hit[K]
		first int
	}
	order := make([]K, 0)
	states := make(map[K]*state)
	seq := 0
	for _, leg := range legs {
		seen := make(map[K]struct{}, len(leg.Keys))
		rank := 0
		for _, key := range leg.Keys {
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			rank++
			term := leg.Weight / (k + float64(rank))
			if err := finiteValue(term); err != nil {
				return nil, err
			}
			item := states[key]
			if item == nil {
				seq++
				item = &state{hit: Hit[K]{Key: key}, first: seq}
				states[key] = item
				order = append(order, key)
			}
			if err := finiteValue(item.hit.Score + term); err != nil {
				return nil, err
			}
			item.hit.Score += term
			item.hit.Contributions = append(item.hit.Contributions, Contribution{
				Leg: leg.Name, Rank: rank, Weight: leg.Weight, Term: term,
			})
		}
	}
	slices.SortStableFunc(order, func(a, b K) int {
		left, right := states[a], states[b]
		if left == nil || right == nil {
			return 0
		}
		if left.hit.Score > right.hit.Score {
			return -1
		}
		if left.hit.Score < right.hit.Score {
			return 1
		}
		return left.first - right.first
	})
	hits := make([]Hit[K], len(order))
	for i, key := range order {
		state := states[key]
		if state == nil {
			continue
		}
		hits[i] = state.hit
	}
	return hits, nil
}

// Alternate is one member of a group found by one leg.
type Alternate[M comparable] struct {
	Member M
	Leg    string
}

// GroupHit is a fused group. Alternates are retained in discovery order and
// are not deduplicated across legs. Eligibility is the caller's decision.
type GroupHit[G Key, M comparable] struct {
	Group         G
	Score         float64
	Contributions []Contribution
	Alternates    []Alternate[M]
}

// Group is one ranked group and the members observed in that occurrence.
type Group[G Key, M comparable] struct {
	Key     G
	Members []M
}

// GroupLeg is one ranked list of groups, best group first.
type GroupLeg[G Key, M comparable] struct {
	Name   string
	Weight float64
	Groups []Group[G, M]
}

// FuseGroups ranks each group once per leg and keeps every alternate member.
// A later duplicate of the same group in one leg does not add another
// contribution; its members are appended. A member that is not equal to
// itself, such as a struct holding NaN, or that holds an uncomparable
// interface value, is an error and is not stored.
func FuseGroups[G Key, M comparable](k float64, legs []GroupLeg[G, M]) ([]GroupHit[G, M], error) {
	if err := ValidateLegs(k, len(legs), func(i int) (string, float64) {
		return legs[i].Name, legs[i].Weight
	}); err != nil {
		return nil, err
	}
	type state struct {
		hit   GroupHit[G, M]
		first int
	}
	order := make([]G, 0)
	states := make(map[G]*state)
	seq := 0
	for _, leg := range legs {
		seen := make(map[G]struct{}, len(leg.Groups))
		rank := 0
		for _, group := range leg.Groups {
			for _, member := range group.Members {
				if err := checkMember(member); err != nil {
					return nil, err
				}
			}
			_, ranked := seen[group.Key]
			if !ranked {
				seen[group.Key] = struct{}{}
				rank++
			}
			item := states[group.Key]
			if item == nil {
				seq++
				item = &state{hit: GroupHit[G, M]{Group: group.Key}, first: seq}
				states[group.Key] = item
				order = append(order, group.Key)
			}
			if !ranked {
				term := leg.Weight / (k + float64(rank))
				if err := finiteValue(term); err != nil {
					return nil, err
				}
				if err := finiteValue(item.hit.Score + term); err != nil {
					return nil, err
				}
				item.hit.Score += term
				item.hit.Contributions = append(item.hit.Contributions, Contribution{
					Leg: leg.Name, Rank: rank, Weight: leg.Weight, Term: term,
				})
			}
			for _, member := range group.Members {
				item.hit.Alternates = append(item.hit.Alternates, Alternate[M]{Member: member, Leg: leg.Name})
			}
		}
	}
	slices.SortStableFunc(order, func(a, b G) int {
		left, right := states[a], states[b]
		if left == nil || right == nil {
			return 0
		}
		if left.hit.Score > right.hit.Score {
			return -1
		}
		if left.hit.Score < right.hit.Score {
			return 1
		}
		return left.first - right.first
	})
	hits := make([]GroupHit[G, M], len(order))
	for i, key := range order {
		state := states[key]
		if state == nil {
			continue
		}
		hits[i] = state.hit
	}
	return hits, nil
}

// FuseGroupsEvery is FuseGroups restricted to groups that every leg found.
// Use it when each leg is one required concept and a group must show evidence for
// all of them, even when different members carry that evidence. Scores,
// contributions, and alternates match FuseGroups, so the surviving groups
// keep reciprocal-rank order. Each leg's best member for a group is that
// leg's first alternate.
func FuseGroupsEvery[G Key, M comparable](k float64, legs []GroupLeg[G, M]) ([]GroupHit[G, M], error) {
	hits, err := FuseGroups(k, legs)
	if err != nil {
		return nil, err
	}
	// FuseGroups adds at most one contribution per leg, and leg names are
	// unique, so a full set of contributions means every leg found the group.
	return slices.DeleteFunc(hits, func(hit GroupHit[G, M]) bool {
		return len(hit.Contributions) != len(legs)
	}), nil
}

// ValidateLegs checks fusion arguments without fusing. k must be finite and
// positive. leg returns the name and weight of leg i; every leg needs a
// unique, non-empty name and a finite, positive weight. Callers that run
// queries to build legs can reject bad arguments before any query runs.
func ValidateLegs(k float64, n int, leg func(i int) (name string, weight float64)) error {
	if !positiveFinite(k) {
		return errors.New("rrf: k must be positive")
	}
	names := make(map[string]struct{}, n)
	for i := range n {
		name, weight := leg(i)
		if name == "" {
			return errors.New("rrf: leg name is required")
		}
		if _, ok := names[name]; ok {
			return fmt.Errorf("rrf: duplicate leg %q", name)
		}
		names[name] = struct{}{}
		if !positiveFinite(weight) {
			return fmt.Errorf("rrf: leg %q weight must be positive", name)
		}
	}
	return nil
}

func positiveFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v > 0
}

func finiteValue(v float64) error {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return errors.New("rrf: non-finite value cannot be stored")
	}
	return nil
}

// checkMember reports a member that cannot be compared. NaN is not equal to
// itself, and neither is a struct or array that contains NaN. An interface
// holding a slice, map or func panics on comparison; that becomes an error.
func checkMember[M comparable](value M) (err error) {
	defer func() {
		if recover() != nil {
			err = fmt.Errorf("rrf: member of type %T is not comparable", value)
		}
	}()
	other := value
	if value != other {
		return errors.New("rrf: NaN member cannot be stored")
	}
	return nil
}
