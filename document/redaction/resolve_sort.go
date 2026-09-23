package redaction

import "math/bits"

// chargeSort reserves the worst-case comparator calls made by heapSort.
// There are floor(n/2) build sifts and n-1 extraction sifts. Each visits at
// most ceil(log2(n)) levels with at most two comparisons per level, so
// 3*n*ceil(log2(n)) bounds the whole sort, independent of input order.
func (budget *resolveWorkBudget) chargeSort(length int) error {
	if length < 0 {
		return &Problem{Code: problemRenderLimit}
	}
	if length < 2 {
		return nil
	}
	perElement := 3 * bits.Len(uint(length-1))
	if length > (maxResolveWork-budget.used)/perElement {
		return &Problem{Code: problemRenderLimit}
	}
	return budget.spend(length * perElement)
}

// heapSort sorts in place with constant auxiliary space. Resolve callers must
// reserve chargeSort(len(values)) on their shared budget before calling it.
func heapSort[E any](values []E, compare func(E, E) int) {
	sift := func(root, end int) {
		// Testing the parent index first keeps 2*root+1 and child+1 within
		// [0, end], even when the slice length approaches the maximum int.
		for root < end/2 {
			child := 2*root + 1
			if child+1 < end && compare(values[child], values[child+1]) < 0 {
				child++
			}
			if compare(values[root], values[child]) >= 0 {
				return
			}
			values[root], values[child] = values[child], values[root]
			root = child
		}
	}
	for root := len(values)/2 - 1; root >= 0; root-- {
		sift(root, len(values))
	}
	for end := len(values) - 1; end > 0; end-- {
		values[0], values[end] = values[end], values[0]
		sift(0, end)
	}
}
