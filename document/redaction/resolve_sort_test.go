package redaction

import (
	"cmp"
	"math"
	"math/rand/v2"
	"slices"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolverSortComparisonBound(t *testing.T) {
	random := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // Reproducible synthetic sort inputs, not security randomness.
	for _, count := range []int{0, 1, 2, 3, 4, 5, 7, 8, 9, 15, 16, 17, 31, 32, 33, 255, 256, 257, 4000, 8000} {
		for _, pattern := range []string{"ascending", "descending", "equal", "duplicates", "organ pipe", "random"} {
			t.Run(strconv.Itoa(count)+"/"+pattern, func(t *testing.T) {
				values := make([]int, count)
				for index := range values {
					switch pattern {
					case "ascending":
						values[index] = index
					case "descending":
						values[index] = count - index
					case "equal":
						values[index] = 1
					case "duplicates":
						values[index] = index % 3
					case "organ pipe":
						values[index] = min(index, count-index)
					case "random":
						values[index] = random.IntN(max(1, count))
					}
				}
				want := slices.Clone(values)
				slices.Sort(want)
				work := &resolveWorkBudget{}
				require.NoError(t, work.chargeSort(len(values)))
				comparisons := 0
				heapSort(values, func(a, b int) int {
					comparisons++
					return cmp.Compare(a, b)
				})
				require.Equal(t, want, values)
				require.LessOrEqual(t, comparisons, work.used, "the reservation must cover every comparator call")
			})
		}
	}
}

func TestResolverSortBudgetRejectsOverflowWithoutSpending(t *testing.T) {
	for _, length := range []int{-1, math.MaxInt, math.MaxInt / 2} {
		work := &resolveWorkBudget{used: 7}
		var problem *Problem
		require.ErrorAs(t, work.chargeSort(length), &problem)
		require.Equal(t, problemRenderLimit, problem.Code)
		require.Equal(t, 7, work.used)
	}
	work := &resolveWorkBudget{used: maxResolveWork}
	require.NoError(t, work.chargeSort(0))
	require.NoError(t, work.chargeSort(1))
	var problem *Problem
	require.ErrorAs(t, work.chargeSort(2), &problem)
	require.Equal(t, problemRenderLimit, problem.Code)
	require.Equal(t, maxResolveWork, work.used)
}

func TestResolverSortReservationsAccumulate(t *testing.T) {
	work := &resolveWorkBudget{}
	require.NoError(t, work.chargeSort(4000))
	first := work.used
	require.NoError(t, work.chargeSort(8000), "the 8,000-box closure of an ordinary text selection fits")
	require.Greater(t, work.used, first)
	require.NoError(t, work.spend(maxResolveWork-work.used))
	var problem *Problem
	require.ErrorAs(t, work.chargeSort(4000), &problem)
	require.Equal(t, problemRenderLimit, problem.Code)
	require.Equal(t, maxResolveWork, work.used)
}

func TestResolverDecisionOrderingPreservesValidation(t *testing.T) {
	first := validDecision("11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222")
	second := validDecision("33333333-3333-4333-8333-333333333333", "22222222-2222-4222-8222-222222222222")
	input := []Decision{second, first}
	ordered, err := canonicalDecisionOrder(input, &resolveWorkBudget{})
	require.NoError(t, err)
	require.Equal(t, []Decision{first, second}, ordered)
	require.Equal(t, []Decision{second, first}, input, "resolution must not reorder caller-owned decisions")

	_, err = canonicalDecisionOrder([]Decision{first, first}, &resolveWorkBudget{})
	require.Error(t, err, "duplicate IDs remain invalid")
	first.Action = "unknown"
	_, err = canonicalDecisionOrder([]Decision{first}, &resolveWorkBudget{})
	require.Error(t, err, "decision validation must precede resolution")
}
