package notemarkdown

import (
	"context"
	"sort"
)

// A change point starts an unchanged piece of original bytes in the compact
// link view. Only continuation prefixes disappear; destinations never cross a
// physical line, and decoding still happens exactly once in linkScanner.emit.
type linkSourceOffset struct{ original, view int }

func prepareLinkSource(ctx context.Context, src []byte, blocks []sourceBlock, b *budget) ([]byte, []sourceBlock, []linkSourceOffset, error) {
	removed := 0
	for _, block := range blocks {
		if err := b.step(ctx, 1); err != nil {
			return nil, nil, nil, err
		}
		if block.continuation {
			removed += block.content.Start - block.span.Start
		}
	}
	if removed == 0 {
		return src, blocks, nil, nil
	}
	if err := b.reserveStrings(len(src) - removed); err != nil {
		return nil, nil, nil, err
	}
	view := make([]byte, 0, len(src)-removed)
	offsets := []linkSourceOffset{{}}
	// Copy in bounded chunks, so this extra source pass remains cancellable.
	copyPiece := func(start, end int) error {
		for start < end {
			next := min(end, start+1024)
			if err := b.step(ctx, next-start); err != nil {
				return err
			}
			view = append(view, src[start:next]...)
			start = next
		}
		return nil
	}
	last := 0
	for _, block := range blocks {
		if !block.continuation {
			continue
		}
		if err := b.node(ctx, 1); err != nil {
			return nil, nil, nil, err
		}
		if err := copyPiece(last, block.span.Start); err != nil {
			return nil, nil, nil, err
		}
		last = block.content.Start
		offsets = append(offsets, linkSourceOffset{original: last, view: len(view)})
	}
	if err := copyPiece(last, len(src)); err != nil {
		return nil, nil, nil, err
	}
	viewPosition := func(pos int) int {
		i := sort.Search(len(offsets), func(i int) bool { return offsets[i].original > pos }) - 1
		return pos - offsets[i].original + offsets[i].view
	}
	joined := make([]sourceBlock, 0, len(blocks))
	for _, block := range blocks {
		if err := b.step(ctx, 1); err != nil {
			return nil, nil, nil, err
		}
		block.content = Span{viewPosition(block.content.Start), viewPosition(block.content.End)}
		if block.continuation {
			joined[len(joined)-1].content.End = block.content.End
		} else {
			joined = append(joined, block)
		}
	}
	return view, joined, offsets, nil
}

func originalLinkSpan(span Span, offsets []linkSourceOffset) Span {
	if len(offsets) == 0 {
		return span
	}
	start := sort.Search(len(offsets), func(i int) bool { return offsets[i].view > span.Start }) - 1
	end := start
	if span.End > span.Start {
		// A half-open end at a removed prefix belongs to the preceding piece.
		end = sort.Search(len(offsets), func(i int) bool { return offsets[i].view >= span.End }) - 1
	}
	return Span{span.Start + offsets[start].original - offsets[start].view,
		span.End + offsets[end].original - offsets[end].view}
}
