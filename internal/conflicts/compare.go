package conflicts

import (
	"regexp"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Passage is a text excerpt and its caller-provided document identity.
type Passage struct{ Document, Text string }

// Input contains passages in rank order. At most 20 passages are examined.
type Input struct {
	Query    string
	Passages []Passage
}
type Status string

const (
	Conflicting   Status = "CONFLICTING"
	NoConflict    Status = "NO_CONFLICT"
	NotComparable Status = "NOT_COMPARABLE"
)

// Side identifies the exact figure and quote in a passage.
type Side struct {
	Passage int
	Figure  Figure
}

// Conflict is a single conflicting pair, not a transitive group.
type Conflict struct {
	Kind, Unit     string
	Subject        []string
	PeriodsOverlap string
	Sides          [2]Side
}

// MaxComparisons bounds eligible cross-document figure pairs examined by Find.
const MaxComparisons = 10000

// MaxConflicts bounds the conflicting pair edges retained by Find.
const MaxConflicts = 100

type Stats struct {
	PassagesRead, PassagesWithFigures, AlignedPairs, AgreeingPairs int
	Comparisons                                                    int
}
type Report struct {
	Status Status
	// Truncated means passage, comparison-work, or conflict-output limits prevented
	// a complete comparison. Without a found conflict, Status is NOT_COMPARABLE.
	Truncated bool
	Conflicts []Conflict
	Skipped   map[int]string
	Stats     Stats
}

type words map[string]bool

var stopWords = wordSet("a an the is are was were be been being of for to from in on at by with as and or that this it its their has have had will can may does do up than no more less most least within effective since starting beginning until till through ending before date")
var subjectWordRE = regexp.MustCompile(`[\p{L}\p{Nd}]+`)

// Rate conversion is deliberately unsupported; the full denominator remains
// part of the comparison signature.
var rateDenominatorRE = regexp.MustCompile(`(?i)^\s*(?:["'’”»]+\s*)?(?:(?:per|each)\s+|/\s*|(?:a|an)\s+)((?:(` + numRange + `)\s+)?(?:(?:` + rateDenominatorModifier + `)\s+)?[\p{L}\p{Nd}]+)`)
var rateDenominatorContinuationRE = regexp.MustCompile(`(?i)^\s+([\p{L}\p{Nd}]+)\b`)
var rateArticleContinuationRE = regexp.MustCompile(`(?i)^\s*(?:[,;:]\s*)?(?:and\s+)?(?:[(\["'‘’“”«»]\s*)*(?:a|an)\b\s*(?:[(\["'‘’“”«»]\s*)*[\p{L}\p{Nd}]+\b`)
var wrappedArticleRateRE = regexp.MustCompile(`(?i)^\s*(?:["'’”»]+\s*)?(?:a|an)\s*(?:(?:\(\s*[\p{L}\p{Nd}]+\s*\))|(?:\[\s*[\p{L}\p{Nd}]+\s*\])|(?:["'‘“«]\s*[\p{L}\p{Nd}]+\s*["'’”»]))`)
var rateDenominatorExtensionRE = regexp.MustCompile(`(?i)^\s+(?:[\(\[\{\"'‘’“”«»]\s*)?(?:of|for|in|to|from|by|with|on|at|and|or)\s+(?:(?:a|an|the)\s+)?[\p{L}\p{Nd}]+\b`)
var rateFractionContinuationRE = regexp.MustCompile(`(?i)^\s*(?:and\s+)?(?:a\s+)?(?:` + fractionWords + `)\b`)
var rateArithmeticContinuationRE = regexp.MustCompile(`(?i)^\s*(?:[+*/×÷−]|-\s+)\s*(?:[$€£¥]\s*)?[0-9]`)
var rateDenominatorContinuationWords = wordSet("apply applies applied bill bills billed charge charges charged cost costs cover covers covered expire expires include includes included start starts started end ends ended available")
var rateDeterminers = wordSet("my your his her its our their this that these those some any every")
var rateMarkerRE = regexp.MustCompile(`(?i)\b(?:per|each|every)\b|/`)
var contrasts = wordSet("new existing old current previous first second third last minimum maximum monthly annual yearly daily weekly per each additional extra business personal domestic international free paid")

func wordSet(s string) words {
	m := words{}
	for w := range strings.FieldsSeq(s) {
		m[w] = true
	}
	return m
}
func tokens(s string) []string {
	s = norm.NFKC.String(s)
	return strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}
func normalized(s string) string { return singular(strings.ToLower(s)) }

func normalizedRateDenominatorPart(s string) string {
	lower := strings.ToLower(s)
	if lower == "s" || lower == "ms" {
		return lower
	}
	return normalized(s)
}

func content(s string) words {
	m := words{}
	for _, w := range tokens(s) {
		n := normalized(w)
		if n != "" && !stopWords[strings.ToLower(w)] && !stopWords[n] {
			m[n] = true
		}
	}
	return m
}

type subject struct {
	words, discriminators words
	clauseWords           words
	denominator           string
	isDenominator         bool
	unsupportedRate       bool
}

func makeSubject(f Figure, figures []Figure) subject {
	// Use extraction's normalization boundaries to translate rune spans without
	// replacing malformed UTF-8 or splitting a normalization segment.
	text, offsets := normalizeClause(f.Clause)
	normalizedOffsets := make(map[int]int, len(offsets))
	for normalizedByte, originalByte := range offsets {
		normalizedOffsets[originalByte] = normalizedByte
	}
	runeBytes := make([]int, 0, len(f.Clause)+1)
	for originalByte := range f.Clause {
		runeBytes = append(runeBytes, originalByte)
	}
	runeBytes = append(runeBytes, len(f.Clause))
	s := subject{words: words{}, discriminators: words{}}
	ownStart := normalizedOffsets[runeBytes[f.Start-f.ClauseStart]]
	ownEnd := normalizedOffsets[runeBytes[f.End-f.ClauseStart]]
	// Removing a leading figure must not make the following word clause-initial.
	initialToken := -1
	if first := subjectWordRE.FindStringIndex(text); first != nil {
		initialToken = first[0]
	}
	masked := []byte(text)
	var rates [][2]int
	invalidRate := false
	erase := func(a, b int) {
		for i := a; i < b; i++ {
			masked[i] = ' '
		}
	}
	for _, other := range figures {
		if other.ClauseStart != f.ClauseStart || other.ClauseEnd != f.ClauseEnd {
			continue
		}
		a, b := other.Start-f.ClauseStart, other.End-f.ClauseStart
		if a >= 0 && b < len(runeBytes) && a <= b {
			start, startOK := normalizedOffsets[runeBytes[a]]
			end, endOK := normalizedOffsets[runeBytes[b]]
			if startOK && endOK {
				erase(start, end)
				if other.Qualifier != "" {
					if qualifierStart, qualifierEnd, ok := subjectQualifierSpan(text[:start], other.Qualifier); ok {
						erase(qualifierStart, qualifierEnd)
					}
				}
				suffixStart := end
				if other.Kind == durationKind && strings.HasPrefix(text[suffixStart:], ".") && durationAbbrevRE.MatchString(text[:suffixStart+1]) {
					suffixStart++
				}
				if start == ownStart && end == ownEnd && wrappedArticleRateRE.MatchString(text[suffixStart:]) {
					invalidRate = true
				}
				if match := rateDenominatorRE.FindStringSubmatchIndex(text[suffixStart:]); match != nil {
					a, b := suffixStart+match[2], suffixStart+match[3]
					parts := strings.Fields(text[a:b])
					noun := parts[len(parts)-1]
					first := utfFirst(noun)
					// An abbreviated composite prefix such as "sq." or "cu." is not a complete denominator.
					incompleteCompositePrefix := strings.EqualFold(noun, "sq") || strings.EqualFold(noun, "cu")
					if incompleteCompositePrefix || strings.EqualFold(parts[0], "per") || strings.EqualFold(parts[0], "each") || !unicode.IsLetter(first) || numeralWord(noun) || stopWords[strings.ToLower(noun)] || rateDeterminers[strings.ToLower(noun)] || contrasts[strings.ToLower(noun)] {
						invalidRate = true
						continue
					}
					rates = append(rates, [2]int{suffixStart + match[0], suffixStart + match[1]})
					if start == ownStart && end == ownEnd {
						if match[4] >= 0 {
							numberStart, numberEnd := suffixStart+match[4], suffixStart+match[5]
							first, last, ok := numbers(text[numberStart:numberEnd], 1)
							if !ok {
								invalidRate = true
								continue
							}
							number := first
							if last != "" {
								number += "-" + last
							}
							parts = append([]string{number}, strings.Fields(text[numberEnd:b])...)
						}
						for i, part := range parts {
							parts[i] = normalizedRateDenominatorPart(part)
						}
						s.denominator = strings.Join(parts, " ")
					}
					// A numeric duration/count within a rate is a denominator,
					// not an independently comparable assertion.
					if ownStart >= a && ownEnd <= b {
						s.isDenominator = true
					}
					continuation := text[suffixStart+match[1]:]
					if strings.HasPrefix(continuation, ".") && durationAbbrevRE.MatchString(text[a:b]+".") {
						fields := strings.Fields(continuation[1:])
						if len(fields) > 1 && (fields[0] == "a" || fields[0] == "an") {
							continuation = continuation[1:]
						}
					}
					acceptedDateBound := acceptedRateDateBound(text, suffixStart+match[1], f)
					unsupportedExtension := rateDenominatorExtensionRE.MatchString(continuation) && !acceptedDateBound
					if rateFractionContinuationRE.MatchString(continuation) || rateArithmeticContinuationRE.MatchString(continuation) || rateArticleContinuationRE.MatchString(continuation) || unsupportedExtension {
						invalidRate = true
					}
					if !acceptedDateBound {
						if word := rateDenominatorContinuationRE.FindStringSubmatch(continuation); len(word) > 1 {
							lower := strings.ToLower(word[1])
							if !stopWords[lower] && !rateDenominatorContinuationWords[lower] {
								invalidRate = true
							}
						}
					}
				}
			}
		}
	}
	// Dates omitted as values still have extraction's accepted spans. Rejected
	// date-shaped text must retain its subject words and digit discriminators.
	for _, span := range f.acceptedDates {
		erase(span.start, span.end)
	}
	// Accepted rate spans are represented by their denominator signature;
	// their marker spelling must not become a subject discriminator.
	for _, span := range rates {
		erase(span[0], span[1])
	}
	// Unbound markers (including prefix rates) and multiple rates exceed the
	// suffix grammar. Omit them rather than compare their amounts or counts.
	s.unsupportedRate = invalidRate || len(rates) > 1 || rateMarkerRE.Match(masked)
	text = string(masked)
	for _, span := range subjectWordRE.FindAllStringIndex(text, -1) {
		w := text[span[0]:span[1]]
		n := normalized(w)
		if n == "" || stopWords[strings.ToLower(w)] || stopWords[n] {
			continue
		}
		s.words[n] = true
		digit, upper, letters := false, true, 0
		for _, c := range w {
			if unicode.IsDigit(c) {
				digit = true
			}
			if unicode.IsLetter(c) {
				letters++
				if !unicode.IsUpper(c) {
					upper = false
				}
			}
		}
		first := utfFirst(w)
		if digit || (upper && letters >= 2) || (span[0] >= initialToken && unicode.IsUpper(first)) || contrasts[strings.ToLower(w)] || contrasts[n] {
			s.discriminators[n] = true
		}
	}
	// Count adjectives are part of the extracted span and must remain constraints.
	if f.Modifier != "" {
		n := normalized(f.Modifier)
		s.words[n] = true
		s.discriminators[n] = true
	}
	return s
}

func acceptedRateDateBound(text string, start int, f Figure) bool {
	if start < 0 || start > len(text) || f.Period.From == "" && f.Period.To == "" {
		return false
	}
	for _, span := range f.acceptedDates {
		if span.start < start || span.end < span.start || span.end > len(text) {
			continue
		}
		lead := strings.TrimFunc(text[start:span.start], func(r rune) bool {
			return unicode.IsSpace(r) || strings.ContainsRune(`.,;:()[]{}"'‘’“”«»`, r)
		})
		lead = strings.Join(strings.Fields(lead), " ")
		for _, word := range periodWords {
			if strings.EqualFold(lead, word) {
				return true
			}
		}
	}
	return false
}

func subjectQualifierSpan(prefix, qualifier string) (int, int, bool) {
	normalizedPrefix := strings.ToLower(strings.Join(strings.Fields(prefix), " "))
	before, found := splitQualifier(normalizedPrefix)
	if found == "" || found != qualifier {
		return 0, 0, false
	}
	qualifierWords := subjectWordRE.FindAllString(strings.TrimSpace(strings.TrimPrefix(normalizedPrefix, before)), -1)
	if len(qualifierWords) == 0 {
		return 0, 0, false
	}
	tokens := subjectWordRE.FindAllStringIndex(prefix, -1)
	if len(tokens) < len(qualifierWords) {
		return 0, 0, false
	}
	first := len(tokens) - len(qualifierWords)
	for i, word := range qualifierWords {
		if !strings.EqualFold(prefix[tokens[first+i][0]:tokens[first+i][1]], word) {
			return 0, 0, false
		}
	}
	return tokens[first][0], tokens[len(tokens)-1][1], true
}

func utfFirst(s string) rune {
	for _, r := range s {
		return r
	}
	return 0
}
func compatible(a, b Figure) bool {
	return a.Kind == b.Kind && a.Unit == b.Unit && a.Qualifier == b.Qualifier && (a.EndValue == "") == (b.EndValue == "")
}
func aligned(a, b subject, query words, identifier bool) bool {
	if a.isDenominator || b.isDenominator || a.unsupportedRate || b.unsupportedRate || a.denominator != b.denominator {
		return false
	}
	common := 0
	for w := range a.words {
		if b.words[w] {
			common++
		}
		if !b.words[w] && a.discriminators[w] {
			return false
		}
	}
	for w := range b.words {
		if !a.words[w] && b.discriminators[w] {
			return false
		}
	}
	if len(query) > 0 {
		found := false
		for w := range query {
			if a.clauseWords[w] && b.clauseWords[w] {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	union := len(a.words) + len(b.words) - common
	if union == 0 {
		return identifier
	}
	if common*2 < union {
		return false
	}
	return !identifier || common == union
}
func equalValue(a, b Figure) bool {
	if a.Kind == "date" && (a.Precision == "month" || b.Precision == "month") {
		return len(a.Value) >= 7 && len(b.Value) >= 7 && a.Value[:7] == b.Value[:7]
	}
	return a.Value == b.Value && a.EndValue == b.EndValue
}
func periodOverlap(a, b Period) string {
	if (a.From == "" && a.To == "") || (b.From == "" && b.To == "") {
		return "unknown"
	}
	// A month bound covers the entire month; conservative overlap avoids false separation.
	lower := func(s string) string {
		if len(s) == 7 {
			return s + "-01"
		}
		return s
	}
	upper := func(s string) string {
		if len(s) == 7 {
			return s + "-31"
		}
		return s
	}
	if a.To != "" && b.From != "" && upper(a.To) < lower(b.From) || b.To != "" && a.From != "" && upper(b.To) < lower(a.From) {
		return "false"
	}
	return "true"
}

// Find compares eligible figures across different documents in passage/figure
// order. It examines at most 20 passages and MaxComparisons eligible pairs and
// retains at most MaxConflicts edges. Truncated reports incomplete comparison or
// output; NO_CONFLICT requires a complete comparison with at least one alignment.
func Find(in Input) Report {
	report := Report{Status: NotComparable, Skipped: map[int]string{}}
	passages := in.Passages
	if len(passages) > 20 {
		passages = passages[:20]
		report.Truncated = true
	}
	report.Stats.PassagesRead = len(passages)
	figures := make([][]Figure, len(passages))
	subjects := make([][]subject, len(passages))
	query := content(in.Query)
	for i, p := range passages {
		if len(p.Text) > MaxPassageBytes {
			report.Skipped[i] = "too_long"
			continue
		}
		extracted := Extract(p.Text)
		if len(extracted) == 0 {
			report.Skipped[i] = "no_figures"
			if hasNegation(norm.NFKC.String(p.Text)) {
				report.Skipped[i] = "negated"
			}
			continue
		}
		report.Stats.PassagesWithFigures++
		allPartial := true
		for _, f := range extracted {
			if !f.Partial {
				allPartial = false
				break
			}
		}
		if allPartial {
			report.Skipped[i] = "partial"
		}
		// Clause-local figure lists avoid rescanning the whole passage per subject.
		clauses := map[[2]int][]Figure{}
		for _, f := range extracted {
			key := [2]int{f.ClauseStart, f.ClauseEnd}
			clauses[key] = append(clauses[key], f)
		}
		type preparedFigure struct {
			figure  Figure
			subject subject
		}
		var prepared []preparedFigure
		independent := map[[2]int]int{}
		for _, f := range extracted {
			if f.Partial {
				continue
			}
			key := [2]int{f.ClauseStart, f.ClauseEnd}
			s := makeSubject(f, clauses[key])
			if !s.isDenominator {
				independent[key]++
			}
			prepared = append(prepared, preparedFigure{f, s})
		}
		for _, item := range prepared {
			f, s := item.figure, item.subject
			if s.isDenominator || s.unsupportedRate || independent[[2]int{f.ClauseStart, f.ClauseEnd}] > 1 {
				continue
			}
			figures[i] = append(figures[i], f)
			if len(query) > 0 {
				s.clauseWords = content(f.Clause)
			}
			subjects[i] = append(subjects[i], s)
		}
		if len(figures[i]) == 0 && report.Skipped[i] == "" {
			report.Skipped[i] = "ambiguous"
		}
	}
	if in.Query != "" && len(query) == 0 {
		return report
	}
compare:
	for i := range passages {
		for j := i + 1; j < len(passages); j++ {
			if passages[i].Document == passages[j].Document {
				continue
			}
			for x, a := range figures[i] {
				for y, b := range figures[j] {
					if report.Stats.Comparisons == MaxComparisons {
						report.Truncated = true
						break compare
					}
					report.Stats.Comparisons++
					if !compatible(a, b) || !aligned(subjects[i][x], subjects[j][y], query, a.Kind == "identifier") {
						continue
					}
					report.Stats.AlignedPairs++
					if equalValue(a, b) {
						report.Stats.AgreeingPairs++
						continue
					}
					if len(report.Conflicts) == MaxConflicts {
						report.Truncated = true
						break compare
					}
					shared := []string{}
					for w := range subjects[i][x].words {
						if subjects[j][y].words[w] {
							shared = append(shared, w)
						}
					}
					sort.Strings(shared)
					report.Conflicts = append(report.Conflicts, Conflict{Kind: a.Kind, Unit: a.Unit, Subject: shared, PeriodsOverlap: periodOverlap(a.Period, b.Period), Sides: [2]Side{{i, a}, {j, b}}})
				}
			}
		}
	}
	if len(report.Conflicts) > 0 {
		report.Status = Conflicting
	} else if report.Stats.AlignedPairs > 0 && !report.Truncated {
		report.Status = NoConflict
	}
	return report
}
