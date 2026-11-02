package embedfit

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/embedmodel"
)

// Tokenizer counts tokens in the exact string a model will receive.
type Tokenizer interface {
	Count(text string) (int, error)
}

// Policy is the token window. Truncation has no default.
type Policy struct {
	MaxTokens     int
	OverlapTokens int
	MaxSpans      int
	Truncation    embedconfig.Truncation
}

// PolicyFrom copies the token window out of shared input settings.
func PolicyFrom(in embedconfig.InputLimits) Policy {
	return Policy{
		MaxTokens:     in.MaxTokens,
		OverlapTokens: in.OverlapTokens,
		MaxSpans:      in.MaxSpans,
		Truncation:    in.Truncation,
	}
}

// Span is one fitted piece of source text. It never starts or ends with
// blank text, and its coordinates cover only the trimmed text.
type Span struct {
	Text      string
	ByteStart int
	ByteEnd   int
	RuneStart int
	RuneEnd   int
	// Truncated is true when the cut was a hard rune cut.
	Truncated bool
}

// Result is the fitted source. TailDropped is true when source text remains
// after the emitted spans. prefix and suffix are the pair Fit counted.
type Result struct {
	Spans       []Span
	TailDropped bool
	prefix      string
	suffix      string
}

// Prepared is one model input plus the source coordinate it came from.
// Text includes the prefix and suffix. Span does not.
type Prepared struct {
	Index     int
	Text      string
	Span      embedmodel.SourceSpan
	Truncated bool
}

// ErrHardCut reports that the only in-budget cut is not a natural boundary.
var ErrHardCut = errors.New("embed fit would cut inside a token window with no natural boundary")

// ErrTailDropped reports that the policy refuses to leave source text unembedded.
var ErrTailDropped = errors.New("embed fit would drop trailing content")

// ErrInputTooLong reports that the prefix, suffix, and one source rune do not fit.
var ErrInputTooLong = errors.New("embed input does not fit in the token budget")

// Validate checks the token window.
func (p Policy) Validate() error {
	window := embedconfig.InputLimits{
		MaxTokens:     p.MaxTokens,
		OverlapTokens: p.OverlapTokens,
		MaxSpans:      p.MaxSpans,
		Truncation:    p.Truncation,
		Tokenizer:     "fit",
	}
	if p.MaxTokens <= 0 && p.OverlapTokens == 0 && p.MaxSpans == 0 && p.Truncation == "" {
		return errors.New("embed fit token window is required")
	}
	return window.Validate()
}

// Fit splits source so each formatted span stays within the token budget.
// prefix and suffix are counted and are not part of the source coordinates.
// An empty or blank source returns no spans.
func Fit(source, prefix, suffix string, tok Tokenizer, policy Policy) (Result, error) {
	if err := policy.Validate(); err != nil {
		return Result{}, err
	}
	if tok == nil {
		return Result{}, errors.New("embed tokenizer is required")
	}
	if embedmodel.BlankText(source) {
		return Result{prefix: prefix, suffix: suffix}, nil
	}
	offsets := runeOffsets(source)
	first, total := trim(source, offsets, 0, len(offsets)-1)
	whole, err := countRange(tok, prefix, suffix, source, offsets, first, total)
	if err != nil {
		return Result{}, err
	}
	if whole <= policy.MaxTokens {
		return Result{Spans: []Span{spanAt(source, offsets, first, total, false)}, prefix: prefix, suffix: suffix}, nil
	}

	var spans []Span
	cursor := first
	for cursor < total {
		if policy.MaxSpans > 0 && len(spans) >= policy.MaxSpans {
			return finish(spans, true, policy.Truncation, prefix, suffix)
		}
		end, err := farthest(tok, prefix, suffix, source, offsets, cursor, total, policy.MaxTokens)
		if err != nil {
			return Result{}, err
		}
		if end <= cursor {
			if policy.Truncation == embedconfig.TruncationDropTail && len(spans) > 0 {
				return finish(spans, true, policy.Truncation, prefix, suffix)
			}
			return Result{}, ErrInputTooLong
		}
		cut, natural := end, end == total
		if end < total {
			if soft, ok := softEnd(source, offsets, cursor, end, total); ok && soft > cursor {
				cut = soft
				natural = true
			} else {
				natural = false
			}
		}
		if !natural && policy.Truncation == embedconfig.TruncationReject {
			return Result{}, ErrHardCut
		}
		// cursor is not blank, so the trimmed span keeps at least one rune.
		// Trimming only removes a tail, so the span stays within budget.
		start, stop := trim(source, offsets, cursor, cut)
		spans = append(spans, spanAt(source, offsets, start, stop, !natural))
		if cut >= total {
			return Result{Spans: spans, prefix: prefix, suffix: suffix}, nil
		}
		next, err := nextStart(tok, source, offsets, cursor, cut, total, policy.OverlapTokens)
		if err != nil {
			return Result{}, err
		}
		if next <= cursor {
			return Result{}, errors.New("embed fit made no progress")
		}
		cursor, _ = trim(source, offsets, next, total)
	}
	return Result{Spans: spans, prefix: prefix, suffix: suffix}, nil
}

// Prepared formats the spans with the prefix and suffix Fit counted, so
// each text is the one that fit the budget.
func (r Result) Prepared() []Prepared {
	prefix, suffix := r.prefix, r.suffix
	out := make([]Prepared, len(r.Spans))
	for i, span := range r.Spans {
		out[i] = Prepared{
			Index: i,
			Text:  prefix + span.Text + suffix,
			Span: embedmodel.SourceSpan{
				ByteStart: span.ByteStart,
				ByteEnd:   span.ByteEnd,
				RuneStart: span.RuneStart,
				RuneEnd:   span.RuneEnd,
			},
			Truncated: span.Truncated,
		}
	}
	return out
}

func finish(spans []Span, tail bool, truncation embedconfig.Truncation, prefix, suffix string) (Result, error) {
	if tail && truncation == embedconfig.TruncationReject {
		return Result{}, ErrTailDropped
	}
	return Result{Spans: spans, TailDropped: tail, prefix: prefix, suffix: suffix}, nil
}

func farthest(tok Tokenizer, prefix, suffix, source string, offsets []int, start, total, maxTokens int) (int, error) {
	best := start
	lo, hi := start+1, total
	for lo <= hi {
		mid := lo + (hi-lo)/2
		n, err := countRange(tok, prefix, suffix, source, offsets, start, mid)
		if err != nil {
			return 0, err
		}
		if n <= maxTokens {
			best = mid
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	return best, nil
}

func nextStart(tok Tokenizer, source string, offsets []int, start, end, total, overlap int) (int, error) {
	if overlap <= 0 || end >= total {
		return end, nil
	}
	best := end
	lo, hi := start+1, end
	for lo <= hi {
		mid := lo + (hi-lo)/2
		n, err := countRange(tok, "", "", source, offsets, mid, end)
		if err != nil {
			return 0, err
		}
		if n <= overlap {
			best = mid
			hi = mid - 1
		} else {
			lo = mid + 1
		}
	}
	if best <= start || best >= end {
		return end, nil
	}
	if snapped := snapForward(source, offsets, best, end); snapped > start && snapped < end {
		return snapped, nil
	}
	return best, nil
}

func countRange(tok Tokenizer, prefix, suffix, source string, offsets []int, start, end int) (int, error) {
	return countFormatted(tok, prefix, source[offsets[start]:offsets[end]], suffix)
}

func countFormatted(tok Tokenizer, prefix, text, suffix string) (int, error) {
	formatted := prefix + text + suffix
	n, err := tok.Count(formatted)
	if err != nil {
		return 0, fmt.Errorf("embed tokenizer: %w", err)
	}
	if n < 0 {
		return 0, errors.New("embed tokenizer returned a negative count")
	}
	if !embedmodel.BlankText(formatted) && n == 0 {
		return 0, errors.New("embed tokenizer counted zero tokens for non-empty text")
	}
	return n, nil
}

func spanAt(source string, offsets []int, start, end int, truncated bool) Span {
	return Span{
		Text:      source[offsets[start]:offsets[end]],
		ByteStart: offsets[start],
		ByteEnd:   offsets[end],
		RuneStart: start,
		RuneEnd:   end,
		Truncated: truncated,
	}
}

func runeOffsets(source string) []int {
	offsets := make([]int, 0, utf8.RuneCountInString(source)+1)
	for i := range source {
		offsets = append(offsets, i)
	}
	offsets = append(offsets, len(source))
	return offsets
}

// softEnd finds a natural cut for the window [start, end). Every boundary
// ends in a space or newline, so it may look one rune past end: a window
// that ends on "." followed by a space still ends a sentence. A cut at
// end+1 only adds that separator, which the caller trims.
func softEnd(source string, offsets []int, start, end, total int) (int, bool) {
	if end-start < 2 {
		return end, false
	}
	floor := start + (end-start)*3/4
	if floor <= start {
		floor = start + 1
	}
	if floor >= end {
		return end, false
	}
	limit := end
	if end < total {
		limit = end + 1
	}
	// A paragraph break may start one rune before floor; its cut still lands
	// inside the preferred window.
	paragraph := floor
	if paragraph-1 > start {
		paragraph--
	}
	if p := strings.LastIndex(source[offsets[paragraph]:offsets[limit]], "\n\n"); p >= 0 {
		return runeAt(offsets, offsets[paragraph]+p+2), true
	}
	window := source[offsets[floor]:offsets[limit]]
	base := offsets[floor]
	best := -1
	for _, term := range []string{". ", "? ", "! ", ".\n", "?\n", "!\n"} {
		if i := strings.LastIndex(window, term); i >= 0 && i+len(term) > best {
			best = i + len(term)
		}
	}
	if best > 0 {
		return runeAt(offsets, base+best), true
	}
	if i := strings.LastIndexByte(window, ' '); i >= 0 {
		return runeAt(offsets, base+i+1), true
	}
	return end, false
}

// trim narrows [start, end) past blank runes at both ends, using the
// embedmodel.BlankText rule. A range that is all blank returns start == end.
func trim(source string, offsets []int, start, end int) (int, int) {
	for start < end && embedmodel.BlankText(source[offsets[start]:offsets[start+1]]) {
		start++
	}
	for end > start && embedmodel.BlankText(source[offsets[end-1]:offsets[end]]) {
		end--
	}
	return start, end
}

func snapForward(source string, offsets []int, pos, end int) int {
	if isBoundary(source, offsets, pos) {
		return pos
	}
	for i := pos + 1; i < end; i++ {
		if isBoundary(source, offsets, i) {
			return i
		}
	}
	return end
}

func isBoundary(source string, offsets []int, pos int) bool {
	if pos <= 0 {
		return true
	}
	prev := source[offsets[pos-1]:offsets[pos]]
	return prev == " " || strings.HasSuffix(prev, "\n")
}

func runeAt(offsets []int, byteOffset int) int {
	lo, hi := 0, len(offsets)-1
	for lo < hi {
		mid := (lo + hi) / 2
		if offsets[mid] < byteOffset {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}
