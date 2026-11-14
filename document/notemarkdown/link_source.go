package notemarkdown

import "sort"

type linkSourceOffset struct{ original, view int }

func originalLinkSpan(span Span, offsets []linkSourceOffset) Span {
	start := sort.Search(len(offsets), func(i int) bool { return offsets[i].view > span.Start }) - 1
	end := start
	if span.End > span.Start {
		// A half-open end at a removed prefix belongs to the preceding piece.
		end = sort.Search(len(offsets), func(i int) bool { return offsets[i].view >= span.End }) - 1
	}
	return Span{span.Start + offsets[start].original - offsets[start].view,
		span.End + offsets[end].original - offsets[end].view}
}
