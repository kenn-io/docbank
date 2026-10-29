// Package conflicts extracts comparable figures from passage text.
package conflicts

import (
	"math/big"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Period describes inclusive date bounds stated by a clause. Empty bounds are unknown.
type Period struct{ From, To string }

// Figure describes a figure and its clause. All offsets are rune offsets into the input.
// Value and EndValue are exact normalized decimals, dates, or identifier strings;
// EndValue is set only for ranges. Date precision is "day" or "month".
type Figure struct {
	Kind, Unit, Value, EndValue, Qualifier, Precision, Text string
	Start, End                                              int
	Clause                                                  string
	ClauseStart, ClauseEnd                                  int
	Partial                                                 bool
	Period                                                  Period
	// Modifier preserves the adjective in the bounded count grammar.
	Modifier string
	// acceptedDates are byte spans in the normalized clause. They retain the
	// extraction decision for dates omitted from the public figure list.
	acceptedDates []dateSpan
}

type dateSpan struct{ start, end int }

type clause struct {
	start, end int
	partial    bool
}
type candidate struct {
	start, end int
	f          Figure
	priority   int
}

var (
	// Longer number words precede their prefixes (fourteen before four).
	number                 = numberPattern()
	numRange               = `(?:between\s+` + number + `\s+and\s+` + number + `|` + number + `(?:(?:\s*(?:-|–)\s*|\s+to\s+)` + number + `)?)`
	currencyCodes          = `(?:USD|EUR|GBP|JPY|CNY|RUB|CAD|AUD|CHF)`
	moneyRE                = regexp.MustCompile(`(?i)(?:[$€£¥]|\b` + currencyCodes + `\s*)\s*(` + numRange + `)|\b(` + numRange + `)\s*(dollars?|euros?|pounds?|yuan|yen|roubles?|rubles?|` + currencyCodes + `)\b|\b(` + numRange + `)\s*([$€£¥])`)
	currencyCodeRE         = regexp.MustCompile(`(?i)\b` + currencyCodes + `\b`)
	currencyCodePrefixRE   = regexp.MustCompile(`(?i)\b` + currencyCodes + `\s*$`)
	currencyCodeSuffixRE   = regexp.MustCompile(`(?i)^\s*` + currencyCodes + `\b`)
	currencySymbolPrefixRE = regexp.MustCompile(`[$€£¥]\s*$`)
	currencySymbolSuffixRE = regexp.MustCompile(`^\s*[$€£¥]`)
	unitRE                 = regexp.MustCompile(`(?i)\b(` + numRange + `)\s*(percentage\s+points?|percent|per\s+cent|%|business\s+days?|working\s+days?|calendar\s+days?|minutes?|mins?|hours?|hrs?|days?|weeks?|months?|years?)\b|\b(` + numRange + `)\s*(%)`)
	dateRE                 = regexp.MustCompile(`(?i)\b(?:[0-9]{4}-[0-9]{2}-[0-9]{2}|[0-9]{1,2}/[0-9]{1,2}/[0-9]{4}|[0-9]{1,2}\s+(?:january|february|march|april|may|june|july|august|september|october|november|december)\s+[0-9]{4}|(?:january|february|march|april|may|june|july|august|september|october|november|december)\s+(?:[0-9]{1,2},?\s+)?[0-9]{4})\b`)
	identifierLabel        = `(?:account|policy|reference|invoice|order|contract|case|ticket|serial|registration|member|customer|iban|tax\s+id|vat)`
	identifierRE           = regexp.MustCompile(`(?i)\b(` + identifierLabel + `)(?:\s+(?:number|no\.?|id))?\s*(?::|\bis\b)\s*([a-z0-9*][a-z0-9*-]{3,})`)
	identifierExplicitRE   = regexp.MustCompile(`(?i)\b(?:number|no\.?|id)\s*(?::|\bis\b)\s*$`)
	identifierMaskRE       = regexp.MustCompile(`(?i)x{2,}`)
	identifierEndingRE     = regexp.MustCompile(`(?i)^[\s,:(\["'‘’“”]+ending\s+in\s+`)
	identifierSuffixMaskRE = regexp.MustCompile(`(?i:^[\s,:(\["'‘’“”]+ending\s+in\b)|^\s+[A-Z]+(?:[^\pL\pN_]|$)|^\s+[*xX]+[0-9*xX-]*(?:[^\pL\pN_]|$)|^\s*[/_-]\s*(?:[0-9*]|(?i:[A-Z]+(?:-[A-Z]+)*-?)[0-9]|[A-Z]{2,}(?:[.,;!?)]|\s+(?i:and|but|while)\b|$))`)
	// Only listed nouns and modifiers participate: arbitrary prose is not parsed as a count.
	countRE               = regexp.MustCompile(`(?i)\b(` + numRange + `)\s+(?:(free|paid|additional|extra)\s+)?(relocations?|devices?|items?|users?|accounts?|payments?|calls?|visits?|tickets?|orders?|requests?|documents?|connections?)\b`)
	negationRE            = regexp.MustCompile(`(?i)(?:\b(?:not|cannot|never|neither|nor|no|without)\b|n['’‘ʼ]t\b|\bno\s+longer\b|\banything\s+but\b)`)
	approximation         = `(?:about|approximately|around|roughly|nearly|almost|circa|estimated)`
	approximationPrefixRE = regexp.MustCompile(`(?i)(?:\b` + approximation + `|[~≈])(?:\s+(?:exactly|a\s+total\s+of|to\s+be|equal\s+to|at|a|the))?[\s()\[\],:"'‘’“”«»]*$`)
	approximationSuffixRE = regexp.MustCompile(`(?i)^[\s,.()\[\]"'‘’“”»]*(?:` + approximation + `\b|more\s+or\s+less\b|and\s+change(?:\s*[,.;!?)]|$)|[~≈])\s*\)*`)
	fractionWords         = `(?:half|quarter|third|fourth|fifth|sixth|seventh|eighth|ninth|tenth)`
	fractionSuffixRE      = regexp.MustCompile(`(?i)^(?:a\s+)?` + fractionWords + `\b`)
	fractionPrefixRE      = regexp.MustCompile(`(?i)\b(?:(?:a\s+)?` + fractionWords + `s?|[0-9]+\s*/\s*[0-9]+)\s+of\s*$`)
	exampleRE             = regexp.MustCompile(`(?i)(?:\bfor\s+example\b|\be\.g\.|\bsuch\s+as\b|\bfor\s+instance\b)`)
	wordRE                = regexp.MustCompile(`(?i)\b` + number + `\b`)
)

func conflictingSymbol(text, unit string, re *regexp.Regexp) bool {
	match := strings.TrimSpace(re.FindString(text))
	return match != "" && match != unit
}

var identifierExplicitTokenRE = regexp.MustCompile(`(?i)\b(?:` + identifierLabel + `\s+(?:number|no\.?|id)|tax\s+id)\s*(?::|\bis\b)\s*\S+`)

// MaxPassageBytes bounds extraction work. Longer passages are skipped whole.
const MaxPassageBytes = 4096

// MaxClauseMatches bounds recognizer work before candidate relationships are checked.
const MaxClauseMatches = 64

const andWord = "and"
const dateKind = "date"
const durationKind = "duration"
const percentKind = "percent"
const unsupportedNumericDigits = "⁰¹²³⁴⁵⁶⁷⁸⁹₀₁₂₃₄₅₆₇₈₉"

// Extract returns figures ordered by their exact spans. Dates and identifiers take
// precedence over other recognizers; ambiguous overlapping matches are omitted.
func Extract(text string) []Figure {
	if len(text) > MaxPassageBytes {
		return nil
	}
	var result []Figure
clauses:
	for _, cl := range splitClauses(text) {
		original := text[cl.start:cl.end]
		s, offsets := normalizeClause(original)
		if strings.Contains(s, "|") || hasNegation(s) || exampleRE.MatchString(s) || nonLatinOnly(s) {
			continue
		}
		var cs, checks []candidate
		identifierSpans := identifierRE.FindAllStringIndex(s, -1)
		explicitTokens := identifierExplicitTokenRE.FindAllStringIndex(s, -1)
		for _, span := range explicitTokens {
			for span[1] > span[0] {
				last, size := utf8.DecodeLastRuneInString(s[span[0]:span[1]])
				if !strings.ContainsRune(".,;:!?"+closingPunctuation, last) || !tokenEndsAt(s[span[1]-size:]) {
					break
				}
				span[1] -= size
			}
			if loc := moneyRE.FindStringIndex(s[span[0]:]); loc != nil && span[0]+loc[0] < span[1] && span[0]+loc[1] > span[1] {
				span[1] = span[0] + loc[1]
			}
			span[1] = identifierTokenEnd(s, span[1])
		}
		identifierSpans = slices.DeleteFunc(identifierSpans, func(span []int) bool {
			match := identifierRE.FindStringSubmatchIndex(s[span[0]:span[1]])
			token := span[0] + match[4]
			label := s[span[0]:token]
			if identifierExplicitRE.MatchString(label) {
				return false
			}
			for _, re := range []*regexp.Regexp{moneyRE, unitRE, countRE} {
				if loc := re.FindStringIndex(s[token:]); loc != nil && loc[0] == 0 && loc[1] > span[1]-token {
					return true
				}
			}
			return false
		})
		unsupportedNumeric := false
		matches := 0
		add := func(re *regexp.Regexp, priority int, parse func(string, []string) (Figure, bool)) {
			if matches > MaxClauseMatches {
				return
			}
			search := s
			if re == moneyRE {
				search = maskDateShapes(s)
			}
			for _, loc := range re.FindAllStringSubmatchIndex(search, -1) {
				matches++
				if matches > MaxClauseMatches {
					return
				}
				start, end := loc[0], loc[1]
				if re == identifierRE && !slices.ContainsFunc(identifierSpans, func(span []int) bool { return span[0] == start && span[1] == end }) {
					continue
				}
				// Resolve contained lower-priority fragments before they can
				// reject the clause as an unsupported numeric expression.
				if (priority > 0 || re == dateRE) && slices.ContainsFunc(explicitTokens, func(span []int) bool {
					return span[0] <= start && start < span[1]
				}) || (priority > 0 || re == dateRE) && slices.ContainsFunc(identifierSpans, func(span []int) bool {
					return identifierOwns(s, span, start, end, priority)
				}) || slices.ContainsFunc(cs, func(c candidate) bool {
					return c.priority < priority && c.start <= start && end <= c.end
				}) {
					continue
				}
				_, startOK := offsets[start]
				if _, endOK := offsets[end]; !startOK || !endOK {
					continue
				}
				r, _ := utf8.DecodeLastRuneInString(s[:start])
				if unicode.IsLetter(r) || unicode.IsDigit(r) || r == ',' || r == '.' {
					if re == dateRE {
						checks = append(checks, candidate{start, end, Figure{Kind: dateKind}, priority})
					}
					unsupportedNumeric = unsupportedNumeric || priority > 0 && (r == ',' || r == '.' || re == moneyRE && currencyCodePrefixRE.MatchString(s[:start]))
					continue
				}
				if end+1 < len(s) && (s[end] == ',' || s[end] == '.') && s[end+1] >= '0' && s[end+1] <= '9' {
					if re == dateRE {
						checks = append(checks, candidate{start, end, Figure{Kind: dateKind}, priority})
					}
					unsupportedNumeric = unsupportedNumeric || priority > 0
					continue
				}
				parts := make([]string, len(loc)/2)
				for i := range parts {
					if loc[2*i] >= 0 {
						parts[i] = s[loc[2*i]:loc[2*i+1]]
					}
				}
				f, ok := parse(s[start:end], parts)
				if !ok || re != identifierRE && (strings.ContainsAny(original[offsets[start]:offsets[end]], unsupportedNumericDigits) || unsupportedOriginalRate(original, offsets, s, end)) {
					if re == dateRE {
						checks = append(checks, candidate{start, end, Figure{Kind: dateKind}, priority})
					}
					unsupportedNumeric = unsupportedNumeric || priority > 0
					continue
				}
				if re == moneyRE && (strings.ContainsAny(f.Unit, "$€£¥") && (currencyCodePrefixRE.MatchString(s[:start]) || currencyCodeSuffixRE.MatchString(s[end:])) ||
					currencyCodeRE.MatchString(f.Unit) && (currencySymbolPrefixRE.MatchString(s[:start]) || currencySymbolSuffixRE.MatchString(s[end:])) ||
					strings.ContainsAny(f.Unit, "$€£¥") && (conflictingSymbol(s[:start], f.Unit, currencySymbolPrefixRE) || conflictingSymbol(s[end:], f.Unit, currencySymbolSuffixRE))) {
					unsupportedNumeric = true
					continue
				}
				if (f.Kind == "identifier" && !identifierEndsAt(s[end:])) || (f.Kind == dateKind && (!dateEndsAt(s[end:]) || dateContinuedPrefix(s[:start]))) {
					if f.Kind == dateKind {
						checks = append(checks, candidate{start, end, f, priority})
					}
					continue
				}
				checks = append(checks, candidate{start, end, f, priority})
				f.Qualifier, ok = qualifier(s[:start])
				if !ok || f.Qualifier != "" && f.EndValue != "" || approximationSuffixRE.MatchString(s[end:]) || (priority > 0 || f.Kind == dateKind) && uncertaintyAdjacent(s[:start], s[end:]) || priority > 0 && (postfixFigureBound(f, s[end:]) || punctuatedBound(s[:start]) || parenthesizedMoney(f, s[:start], s[end:])) {
					prefix := approximationPrefixRE.ReplaceAllString(strings.TrimSpace(s[:start]), "")
					unsupportedNumeric = unsupportedNumeric || !ok && compoundNumberPrefix(prefix)
					continue
				}
				cs = append(cs, candidate{start, end, f, priority})
			}
		}
		add(dateRE, 0, parseDate)
		add(identifierRE, 0, parseIdentifier)
		add(moneyRE, 1, parseMoney)
		add(unitRE, 1, parseUnit)
		add(countRE, 2, parseCount)
		if matches > MaxClauseMatches {
			continue
		}
		cs = omitUncertainGroups(s, checks, cs)
		cs = omitUncertainRates(s, checks, cs)
		cs = omitPercentageBases(s, checks, cs)
		// A partial numeric expression makes the whole clause unsafe, including
		// later figures that would otherwise survive dropping its first part.
		if unsupportedNumeric || unsafeCandidates(s, checks, cs) {
			continue
		}
		sort.SliceStable(cs, func(i, j int) bool { return cs[i].start < cs[j].start })
		rejected := make([]bool, len(cs))
		for i, c := range cs {
			for j := i + 1; j < len(cs) && cs[j].start < c.end; j++ {
				other := cs[j]
				if c.priority >= other.priority {
					rejected[i] = true
				}
				if other.priority >= c.priority {
					rejected[j] = true
				}
			}
		}
		var selected []candidate
		for i, c := range cs {
			if !rejected[i] {
				selected = append(selected, c)
			}
		}
		selected = omitAlternativeStandaloneDates(s, checks, selected)
		period := Period{}
		var acceptedDates []dateSpan
		nonDate := false
		for _, c := range selected {
			if c.f.Kind != dateKind {
				nonDate = true
			} else {
				acceptedDates = append(acceptedDates, dateSpan{c.start, c.end})
			}
		}
		periodUncertain := independentPeriodScopes(s, periodScopeCandidates(selected, checks))
		if nonDate {
			for _, c := range selected {
				if c.f.Kind != dateKind {
					continue
				}
				prefix := trimDateOpening(strings.TrimLeft(strings.ToLower(strings.Join(strings.Fields(s[:c.start]), " ")), `(["'‘’“”`))
				var bound *string
				switch {
				case endsWord(prefix, periodStartWords...):
					bound = &period.From
				case endsWord(prefix, "until", "till", "to", "through", "ending", "before"):
					bound = &period.To
				}
				if bound != nil {
					value := c.f.Value
					periodUncertain = periodUncertain || uncertainDateSuffixRE.MatchString(s[c.end:])
					periodUncertain = periodUncertain || relativeDate(s, checks, selected, c)
					if endsWord(prefix, "before", "prior to") && !endsWord(prefix, "on or before") {
						var ok bool
						value, ok = inclusiveBefore(c.f)
						periodUncertain = periodUncertain || !ok
					}
					if *bound != "" && *bound != value && !hasAlternativeDates(s, checks) {
						continue clauses
					}
					*bound = value
				}
			}
		}
		if periodUncertain || invertedPeriod(period) {
			period = Period{}
		}
		for _, c := range selected {
			if c.f.Kind == dateKind && (nonDate || uncertainDateSuffixRE.MatchString(s[c.end:])) {
				continue
			}
			f := c.f
			start, end := offsets[c.start], offsets[c.end]
			f.Text = original[start:end]
			f.Start = utf8.RuneCountInString(text[:cl.start+start])
			f.End = f.Start + utf8.RuneCountInString(f.Text)
			f.Clause = original
			f.ClauseStart = utf8.RuneCountInString(text[:cl.start])
			f.ClauseEnd = f.ClauseStart + utf8.RuneCountInString(original)
			f.Partial = cl.partial
			f.Period = period
			f.acceptedDates = acceptedDates
			result = append(result, f)
		}
	}
	return result
}

const calendarExclusion = `(?:excluding|excepting|except(?:\s+for)?|(?:apart\s+from|other\s+than)\s+(?:[\p{L}]+\s+){0,3}(?:holidays?|weekends?|christmas|sundays?|mondays?|tuesdays?|wednesdays?|thursdays?|fridays?|saturdays?))\b`
const durationExclusion = `(?:` + calendarExclusion + `|less\b)`

var periodStartWords = []string{"from", "since", "effective", "effective on", "as of", "starting", "starting on", "beginning", "beginning on"}
var periodWords = append(slices.Clone(periodStartWords), "until", "till", "prior to", "to", "through", "ending", "on or before", "before")
var calendarExclusionRE = regexp.MustCompile(`(?i)^[\s,.()\[\]"'‘’“”»]+` + durationExclusion)
var numericExclusionRE = regexp.MustCompile(`(?i)\b` + calendarExclusion + `[^,;0-9]*$`)
var exclusionJoinRE = regexp.MustCompile(`(?i)^[\s,.()\[\]"'‘’“”»]+` + durationExclusion + `(?s:.*?)(\s+and\s+|,\s*)$`)
var dateBeforeConnectorRE = regexp.MustCompile(`(?i)^[\s,()\]"'”’»]+(?:` + durationExclusion + `.*?[\s,()\]"'”’»]+)?(?:before|prior\s+to)\s+[(\["'‘’“”«]*\s*$`)
var dateFromConnectorRE = regexp.MustCompile(`(?i)^[\s,()\]"'”’»]+from\s+[(\["'‘’“”«]*\s*$`)
var uncertainDateSuffixRE = regexp.MustCompile(`(?i)^(?:[\s,()\[\]"'‘’“”«»]+or\s+(?:earlier|later)|[\s,()\[\]"'‘’“”«»]+(?:inclusive|exclusive))\b`)

// A trailing from-date anchors a lone duration unless it starts a stated interval.
func relativeDate(s string, checks, selected []candidate, date candidate) bool {
	bounded := slices.ContainsFunc(selected, func(end candidate) bool {
		return end.f.Kind == dateKind && end.start >= date.end && (dateBoundConnectorRE.MatchString(s[date.end:end.start]) || coordinatedEndRE.MatchString(s[date.end:end.start]))
	})
	return slices.ContainsFunc(checks, func(prev candidate) bool {
		if prev.f.Kind != durationKind || prev.end > date.start {
			return false
		}
		connector := strings.TrimPrefix(s[prev.end:date.start], ".")
		return !rateContains(s, checks, prev) && (dateBeforeConnectorRE.MatchString(connector) || dateFromConnectorRE.MatchString(connector) && !bounded && !slices.ContainsFunc(checks, func(first candidate) bool {
			return first.f.Kind == durationKind && first.end <= prev.start && durationJoined(s[first.end:prev.start])
		}))
	})
}

func hasAlternativeDates(s string, checks []candidate) bool {
	var dates []candidate
	for _, c := range checks {
		if c.f.Kind == dateKind {
			dates = append(dates, c)
		}
	}
	sort.SliceStable(dates, func(i, j int) bool { return dates[i].start < dates[j].start })
	for i := 1; i < len(dates); i++ {
		if dates[i-1].end <= dates[i].start && alternativePeriodRE.MatchString(durationJoinTail(s[dates[i-1].end:dates[i].start])) {
			return true
		}
	}
	return false
}

func standaloneDateRange(s string, first, second candidate) bool {
	before := strings.ToLower(strings.TrimSpace(s[:first.start]))
	connector := strings.ToLower(strings.TrimSpace(s[first.end:second.start]))
	return connector == andWord && endsWord(before, "between") || connector == "to" && !endsWord(before, periodStartWords...)
}

// Alternative standalone dates do not assert either date exactly.
func omitAlternativeStandaloneDates(s string, checks, selected []candidate) []candidate {
	if slices.ContainsFunc(selected, func(c candidate) bool { return c.f.Kind != dateKind }) {
		return selected
	}
	var alternatives, dates []candidate
	for _, c := range checks {
		if c.f.Kind == dateKind {
			dates = append(dates, c)
		}
	}
	sort.SliceStable(dates, func(i, j int) bool { return dates[i].start < dates[j].start })
	for i := 1; i < len(dates); i++ {
		if dates[i-1].end <= dates[i].start && (alternativePeriodRE.MatchString(durationJoinTail(s[dates[i-1].end:dates[i].start])) || standaloneDateRange(s, dates[i-1], dates[i])) {
			alternatives = append(alternatives, dates[i-1], dates[i])
		}
	}
	return slices.DeleteFunc(selected, func(c candidate) bool {
		return slices.ContainsFunc(alternatives, func(a candidate) bool { return a.start == c.start && a.end == c.end })
	})
}

func invertedPeriod(p Period) bool {
	if p.From == "" || p.To == "" {
		return false
	}
	from, to := p.From, p.To
	if len(from) == len("2006-01") {
		from += "-01"
	}
	if len(to) == len("2006-01") {
		t, err := time.Parse("2006-01", to)
		if err != nil {
			return true
		}
		to = t.AddDate(0, 1, -1).Format("2006-01-02")
	}
	return from > to
}

// Scope sees recognized dates and assertions, even those omitted as uncertain.
func periodScopeCandidates(selected, checks []candidate) []candidate {
	var scope []candidate
	for _, c := range checks {
		if slices.ContainsFunc(selected, func(accepted candidate) bool {
			return (accepted.priority < c.priority || accepted.f.Kind == "identifier" && c.f.Kind == dateKind) && accepted.start < c.end && c.start < accepted.end
		}) {
			continue
		}
		if c.f.Kind == dateKind && !slices.ContainsFunc(selected, func(accepted candidate) bool {
			return accepted.f.Kind == dateKind && accepted.start == c.start && accepted.end == c.end
		}) {
			c.f.Value = ""
		}
		scope = append(scope, c)
	}
	sort.SliceStable(scope, func(i, j int) bool { return scope[i].start < scope[j].start })
	return scope
}

var periodPredicateWords = func() string {
	words := slices.Clone(periodWords)
	for i, word := range words {
		words[i] = strings.Join(strings.Fields(word), `\s+`)
	}
	return strings.Join(words, "|")
}()

func trimDateOpening(s string) string {
	return strings.TrimRightFunc(s, func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune(`(["'‘’“”«`, r) })
}

var alternativePeriodRE = regexp.MustCompile(`(?i)^[\s,()\[\]"'‘’“”«»]*or(?:\s+(?:after|on|` + periodPredicateWords + `))?[\s,()\[\]"'‘’“”«»]*$`)

var predicatedPeriodRE = regexp.MustCompile(`(?i)\b[\p{L}\p{N}]+\s+(?:` + periodPredicateWords + `)\s*$`)
var coordinatedPeriodRE = regexp.MustCompile(`(?i)\b(?:and|while|but)\s+(?:` + periodPredicateWords + `)\s*$`)
var qualifiedPeriodRE = regexp.MustCompile(`(?i)\bfor\s+[\p{L}\p{N}.,:'’‘-]+(?:\s+[\p{L}\p{N}.,:'’‘-]+)*\s+(?:` + periodPredicateWords + `)\s*$`)
var conditionalPeriodRE = regexp.MustCompile(`(?i)\b(?:if|when|unless)\s+[\p{L}\p{N}.,:'’‘()-]+(?:\s+[\p{L}\p{N}.,:'’‘()-]+)*\s+(?:` + periodPredicateWords + `)\s*$`)
var coordinatedPredicateRE = regexp.MustCompile(`(?i)\b(?:and|while|but)\s+[\p{L}]+(?:\s+[\p{L}\p{N}.,:'’‘()-]+)*\s+(?:` + periodPredicateWords + `)\s*$`)
var leadingEventPeriodRE = regexp.MustCompile(`(?i)\b(?:[\p{L}]+(?:ed|en|ing)|made|payable|due|who\s+[\p{L}]+)\s+(?:on\s+or\s+before|before|after|prior\s+to|since|from)\s*$`)
var leadingNominalPeriodRE = regexp.MustCompile(`(?i)^\s*[\p{L}\p{N}][\p{L}\p{N}-]*(?:\s+[\p{L}\p{N}][\p{L}\p{N}-]*)*\s+(?:` + periodPredicateWords + `)\s*$`)

var leadingFigureSubjectRE = regexp.MustCompile(`(?i)^\s*(?:(?:the|a|an)\s+)?(?:wait|fee|duration|deposit|rate|price|cost|notice|deadline)\s+(?:` + periodPredicateWords + `)\s*$`)

func unclassifiedLeadingPeriod(s string) bool {
	return !dateRE.MatchString(s) && !slices.Contains(periodWords, strings.ToLower(strings.Join(strings.Fields(s), " "))) && !leadingFigureSubjectRE.MatchString(s) && leadingNominalPeriodRE.MatchString(s)
}

var assertionJoinRE = regexp.MustCompile(`(?i)^(?:[\s,]*(?:and|while|but)\b|\s*,)`)
var subordinatePeriodRE = regexp.MustCompile(`(?i)(?:\bbecause\b|\bsince\b|\()\s*[\p{L}\p{N}.,:'’‘()-]+(?:\s+[\p{L}\p{N}.,:'’‘()-]+)*\s+(?:` + periodPredicateWords + `)\s*$`)
var eligibleSubjectPeriodRE = regexp.MustCompile(`(?i)\bapplies?\s+to\s+[\p{L}\p{N}-]+(?:\s+[\p{L}\p{N}-]+)*\s+(?:` + periodPredicateWords + `)\s*$`)
var coordinatedEndRE = regexp.MustCompile(`(?i)^\s+and\s+ending\s+$`)

// Classify coordinated date phrases, retaining a terminal start-and-end interval.
func coordinatedPeriodScope(s string, firstFigureEnd, lastDateEnd, dateCount int, firstDate, firstConnector string, c candidate) (bool, bool) {
	if firstFigureEnd < 0 || firstFigureEnd > c.start {
		return false, false
	}
	prefix := trimDateOpening(s[firstFigureEnd:c.start])
	if subordinatePeriodRE.MatchString(prefix) || eligibleSubjectPeriodRE.MatchString(prefix) || leadingEventPeriodRE.MatchString(prefix) || coordinatedPredicateRE.MatchString(prefix) || strings.HasPrefix(strings.TrimSpace(prefix), ",") && predicatedPeriodRE.MatchString(prefix) || qualifiedPeriodRE.MatchString(prefix) || conditionalPeriodRE.MatchString(prefix) {
		return true, false
	}
	interval := dateCount == 1 && firstDate != "" && lastDateEnd >= 0 && slices.Contains(periodStartWords, firstConnector) &&
		coordinatedEndRE.MatchString(s[lastDateEnd:c.start]) && strings.TrimFunc(s[c.end:], func(r rune) bool { return unicode.IsSpace(r) || unicode.IsPunct(r) }) == ""
	return false, coordinatedPeriodRE.MatchString(prefix) && !interval
}

// Embedded dates cannot safely scope independently stated figures.
func independentPeriodScopes(s string, selected []candidate) bool {
	for i := 1; i < len(selected); i++ {
		if selected[i-1].end > selected[i].start {
			return true
		}
	}
	firstDate, firstConnector := "", ""
	lastDateEnd := -1
	sameBounds, embeddedDate, predicatedDate, coordinatedDate := true, false, false, false
	coordinatedFirst := false
	dateCount, firstFigureEnd := 0, -1
	for _, c := range selected {
		if c.f.Kind != dateKind {
			if predicatedDate && lastDateEnd >= 0 && lastDateEnd <= c.start {
				tail := strings.Fields(strings.ToLower(strings.TrimLeftFunc(s[lastDateEnd:c.start], func(r rune) bool { return unicode.IsSpace(r) || unicode.IsPunct(r) })))
				if len(tail) > 1 && (tail[0] == andWord || tail[0] == "while" || tail[0] == "but" || tail[0] == "so" || tail[0] == "because" || tail[0] == "if" || tail[0] == "when" || tail[0] == "unless") || dateCount == 1 && strings.HasPrefix(strings.TrimSpace(s[lastDateEnd:c.start]), ",") {
					return true
				}
			}
			if firstFigureEnd < 0 {
				firstFigureEnd = c.end
			}
			continue
		}
		if lastDateEnd >= 0 && (alternativePeriodRE.MatchString(durationJoinTail(s[lastDateEnd:c.start])) || slices.Contains(periodWords, firstConnector) && durationJoined(s[lastDateEnd:c.start]) && firstDate != c.f.Value) {
			return true
		}
		independent, coordinated := coordinatedPeriodScope(s, firstFigureEnd, lastDateEnd, dateCount, firstDate, firstConnector, c)
		if independent {
			return true
		}
		coordinatedDate = coordinatedDate || coordinated
		if dateCount == 0 {
			coordinatedFirst = coordinated
		}
		predicatedDate = predicatedPeriodRE.MatchString(trimDateOpening(s[:c.start]))
		if firstFigureEnd < 0 && (subordinatePeriodRE.MatchString(trimDateOpening(s[:c.start])) && (lastDateEnd < 0 || !plainDateBoundConnectorRE.MatchString(strings.TrimLeft(s[lastDateEnd:c.start], closingPunctuation))) || leadingEventPeriodRE.MatchString(trimDateOpening(s[:c.start])) || conditionalPeriodRE.MatchString(trimDateOpening(s[:c.start])) || qualifiedPeriodRE.MatchString(trimDateOpening(s[:c.start])) || unclassifiedLeadingPeriod(trimDateOpening(strings.TrimLeft(s[:c.start], `(["'‘’“”`)))) {
			return true
		}
		lastDateEnd = c.end
		dateCount++
		sameBounds = sameBounds && c.f.Value != ""
		prefix := trimDateOpening(strings.TrimLeft(strings.ToLower(strings.Join(strings.Fields(s[:c.start]), " ")), `(["'‘’“”`))
		connector := ""
		for _, word := range periodWords {
			if endsWord(prefix, word) {
				connector = word
				break
			}
		}
		if firstDate == "" {
			firstDate, firstConnector, embeddedDate = c.f.Value, connector, prefix != connector
		} else if firstDate != c.f.Value || firstConnector != connector {
			sameBounds = false
		}
	}
	if coordinatedFirst || coordinatedDate && !slices.ContainsFunc(selected, func(c candidate) bool { return c.f.Kind != dateKind && c.start > lastDateEnd }) {
		return true
	}
	if sameBounds && dateCount > 1 {
		for i := 1; i < len(selected) && selected[i].f.Kind != dateKind; i++ {
			if selected[i-1].f.Kind != dateKind {
				words := strings.Fields(strings.ToLower(s[selected[i-1].end:selected[i].start]))
				if len(words) > 1 {
					return true
				}
			}
		}
		for i, c := range selected {
			if c.f.Kind != dateKind || i+1 >= len(selected) || selected[i+1].f.Kind == dateKind || !assertionJoinRE.MatchString(s[c.end:selected[i+1].start]) {
				continue
			}
			for j := i + 2; j < len(selected); j++ {
				if selected[j].f.Kind == dateKind {
					if assertionJoinRE.MatchString(s[selected[j-1].end:selected[j].start]) {
						return true
					}
					break
				}
			}
		}
		for _, c := range selected {
			if c.f.Kind != dateKind && c.start >= lastDateEnd {
				tail := strings.Fields(strings.ToLower(s[lastDateEnd:c.start]))
				if len(tail) > 1 && tail[0] == andWord {
					return true
				}
			}
		}
		return false
	}
	if coordinatedDate {
		return true
	}
	if lastDateEnd > firstFigureEnd || lastDateEnd >= 0 && embeddedDate {
		masked := maskAcceptedDates(s, selected)
		for _, c := range selected {
			if c.f.Kind == dateKind || c.start < firstFigureEnd {
				continue
			}
			for _, word := range strings.FieldsFunc(strings.ToLower(masked[firstFigureEnd:c.start]), func(r rune) bool { return !unicode.IsLetter(r) }) {
				if word != andWord && word != "per" && word != "each" && word != "a" {
					return true
				}
			}
			firstFigureEnd = c.end
		}
	}
	seenFigure := false
	lastDateEnd = -1
	for _, c := range selected {
		if c.f.Kind == dateKind {
			if seenFigure {
				lastDateEnd = c.end
			}
			continue
		}
		if lastDateEnd >= 0 {
			if c.start < lastDateEnd {
				return true
			}
			tail := strings.TrimSpace(strings.ToLower(s[lastDateEnd:c.start]))
			tail = strings.TrimSpace(strings.TrimPrefix(tail, "and "))
			if strings.IndexFunc(tail, unicode.IsLetter) >= 0 {
				return true
			}
			lastDateEnd = -1
		}
		seenFigure = true
	}
	return false
}

func inclusiveBefore(f Figure) (string, bool) {
	layout := "2006-01-02"
	if f.Precision == "month" {
		layout = "2006-01"
	}
	t, err := time.Parse(layout, f.Value)
	if err != nil {
		return "", false
	}
	t = t.AddDate(0, 0, -1)
	if t.Year() < 1 {
		return "", false
	}
	return t.Format("2006-01-02"), true
}

// Identifier precedence owns embedded figures and actual token continuations.
func identifierOwns(s string, span []int, start, end, priority int) bool {
	if span[0] > start || end > identifierTokenEnd(s, span[1]) {
		return false
	}
	parts := identifierRE.FindStringSubmatch(s[span[0]:span[1]])
	_, valid := parseIdentifier("", parts)
	r, _ := utf8.DecodeRuneInString(s[span[1]:])
	ownedGroup := identifierGroupRE.MatchString(parts[2]) && (priority > 0 && moneyRE.MatchString(s[start:end]) || priority == 0 && dateRE.MatchString(s[start:end]))
	if !identifierExplicitRE.MatchString(strings.TrimSuffix(s[span[0]:span[1]], parts[2])) && !valid && !strings.ContainsAny(parts[2], "0123456789*") && !identifierMaskRE.MatchString(parts[2]) && start > span[1] && unicode.IsSpace(r) && !ownedGroup {
		return false
	}
	return priority > 0 || !valid || !identifierEndsAt(s[span[1]:])
}

var identifierConjunctionRE = regexp.MustCompile(`(?i)^\s+(?:and|but|while)(?:[^\pL\pN_]|$)`)
var identifierSeparatorRE = regexp.MustCompile(`^[/_-]$`)
var identifierMaskGroupRE = regexp.MustCompile(`(?i)^[*x]+[0-9*x-]*[.,;:!?]*$`)
var identifierGroupRE = regexp.MustCompile(`^[A-Z]+$`)

// Protect lower-priority matches through the rest of a continued identifier token.
func identifierTokenEnd(s string, end int) int {
	if loc := identifierEndingRE.FindStringIndex(s[end:]); loc != nil {
		start := end + loc[1]
		tail := s[start:]
		n := strings.IndexFunc(tail, unicode.IsSpace)
		if n < 0 {
			n = len(tail)
		}
		for _, re := range []*regexp.Regexp{dateRE, moneyRE} {
			if match := re.FindStringIndex(tail); match != nil && match[0] == 0 && match[1] > n {
				n = match[1]
			}
		}
		return start + n
	}
	if identifierConjunctionRE.MatchString(s[end:]) || tokenEndsAt(s[end:]) && !identifierSuffixMaskRE.MatchString(s[end:]) {
		return end
	}
	for end < len(s) {
		field := strings.TrimLeftFunc(s[end:], unicode.IsSpace)
		start := len(s) - len(field)
		n := strings.IndexFunc(field, unicode.IsSpace)
		if n < 0 {
			n = len(field)
		}
		if start > end && (strings.ContainsRune(",.;:!?", rune(s[end-1])) || identifierConjunctionRE.MatchString(s[end:]) || !strings.ContainsAny(field[:n], "0123456789") && !identifierGroupRE.MatchString(field[:n]) && !identifierMaskGroupRE.MatchString(field[:n]) && !identifierSeparatorRE.MatchString(field[:n])) {
			break
		}
		for _, re := range []*regexp.Regexp{dateRE, moneyRE, countRE, unitRE} {
			if match := re.FindStringIndex(field); match != nil && match[0] == 0 && match[1] > n {
				n = match[1]
			}
		}
		end = start + n
	}
	return end
}

// Mask accepted dates when analyzing period relationships.
func maskAcceptedDates(s string, cs []candidate) string {
	b := []byte(s)
	for _, c := range cs {
		if c.f.Kind == dateKind {
			for i := c.start; i < c.end; i++ {
				b[i] = 0
			}
		}
	}
	return string(b)
}

// Keep date-shaped bytes from being consumed as a numeric range endpoint.
func maskDateShapes(s string) string {
	b := []byte(s)
	for _, span := range dateRE.FindAllStringIndex(s, -1) {
		for i := span[0]; i < span[1]; i++ {
			b[i] = 0
		}
	}
	return string(b)
}

// normalizeClause maps NFKC segment boundaries back to original byte offsets.
// Matches that cut through a normalization segment are discarded.
func normalizeClause(s string) (string, map[int]int) {
	var it norm.Iter
	it.InitString(norm.NFKC, s)
	var b strings.Builder
	offsets := map[int]int{0: 0}
	for !it.Done() {
		segment := it.Next()
		b.Write(segment)
		offsets[b.Len()] = it.Pos()
	}
	return b.String(), offsets
}

// Bound phrases and the identifier abbreviation are not negative claims.
func hasNegation(s string) bool {
	for _, loc := range negationRE.FindAllStringIndex(s, -1) {
		word := strings.ToLower(s[loc[0]:loc[1]])
		tail := s[loc[1]:]
		if word == "no" && (strings.HasPrefix(tail, ".") || noBoundRE.MatchString(tail) || identifierNoPrefixRE.MatchString(s[:loc[0]]) && identifierNoTailRE.MatchString(tail)) {
			continue
		}
		return true
	}
	return false
}

var identifierNoPrefixRE = regexp.MustCompile(`(?i)\b` + identifierLabel + `\s+$`)
var identifierNoTailRE = regexp.MustCompile(`(?i)^\s*(?::|\bis\b)`)
var noBoundRE = regexp.MustCompile(`(?i)^\s+(?:more|less)\s+than\b`)

func nonLatinOnly(s string) bool {
	hasLetter := false
	for _, r := range s {
		if unicode.Is(unicode.Latin, r) {
			return false
		}
		hasLetter = hasLetter || unicode.IsLetter(r)
	}
	return hasLetter
}
func endsWord(s string, words ...string) bool {
	for _, w := range words {
		if s == w || strings.HasSuffix(s, " "+w) {
			return true
		}
	}
	return false
}

var boundWords = []struct {
	words []string
	value string
}{{[]string{"less than or equal to", "fewer than or equal to", "up to", "at most", "no more than", "maximum of", "maximum", "max.", "max"}, "≤"}, {[]string{"greater than or equal to", "more than or equal to", "at least", "minimum of", "minimum", "no less than"}, "≥"}, {[]string{"more than", "greater than", "over", "exceed", "exceeds", "exceeded", "exceeding"}, ">"}, {[]string{"less than", "under", "fewer than"}, "<"}, {[]string{"within"}, "within"}}

func postfixBound(s string) bool {
	return postfixBoundEnd(s) > 0
}

var independentOverRE = regexp.MustCompile(`(?i)^\s+and\s+over\s+`)

func postfixFigureBound(f Figure, s string) bool {
	if loc := independentOverRE.FindStringIndex(s); loc != nil && independentBoundedFigure(s[loc[1]:], f) {
		return false
	}
	return postfixBound(s)
}

var postfixBoundLeadRE = regexp.MustCompile(`(?i)^[\s,.()\[\]"'‘’“”»]*(?:(?:at\s+)?(?:a|the)\s+)?`)

// Compile the shared bound vocabulary once; match indices remain byte offsets.
var postfixBounds = func() []*regexp.Regexp {
	result := []*regexp.Regexp{regexp.MustCompile(`(?i)^(?:upwards|tops|and\s+(?:upwards|up|above|over)|and\s+(?:longer|shorter|greater|less|higher|lower|more)(?:\s*[,.;!?)]|$)|at\s+(?:minimum|maximum))`)}
	for _, q := range boundWords {
		for _, word := range q.words {
			if slices.Contains([]string{"over", "under", "within", "exceed", "exceeds", "exceeded", "exceeding"}, word) {
				continue
			}
			result = append(result, regexp.MustCompile(`(?i)^`+strings.Join(strings.Fields(word), `\s+`)))
		}
	}
	return result
}()

func postfixBoundEnd(s string) int {
	lead := postfixBoundLeadRE.FindStringIndex(s)[1]
	tail := s[lead:]
	for _, re := range postfixBounds {
		if loc := re.FindStringIndex(tail); loc != nil {
			rest := tail[loc[1]:]
			r, _ := utf8.DecodeRuneInString(rest)
			if rest == "" || !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' {
				return lead + loc[1]
			}
		}
	}
	return 0
}

func punctuatedBound(prefix string) bool {
	trimmed := strings.TrimRightFunc(prefix, func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune(`(["'‘’“”`, r) })
	if !strings.ContainsAny(prefix[len(trimmed):], "([") {
		return false
	}
	_, q := splitQualifier(strings.ToLower(strings.Join(strings.Fields(trimmed), " ")))
	return q != ""
}

func parenthesizedMoney(f Figure, prefix, suffix string) bool {
	if f.Kind != "money" || !strings.HasSuffix(strings.TrimSpace(prefix), "(") {
		return false
	}
	if n := rateEnd(suffix); n > 0 {
		suffix = suffix[n:]
	}
	return strings.HasPrefix(strings.TrimSpace(suffix), ")")
}

var uncertaintyPrefixRE = regexp.MustCompile(`(?i)\bgive\s+or\s+take[\s(\["'‘’“”]*$`)
var uncertaintySuffixRE = regexp.MustCompile(`(?i)^[\s.()\[\]"'‘’“”»]*,?\s*give\s+or\s+take\b`)

func uncertaintyAdjacent(prefix, suffix string) bool {
	prefix, _ = splitQualifier(strings.ToLower(strings.Join(strings.Fields(prefix), " ")))
	return uncertaintyPrefixRE.MatchString(prefix) || uncertaintySuffixRE.MatchString(suffix)
}

var nominalBoundSubjectRE = regexp.MustCompile(`(?i)(?:^|,|\b(?:and|but|while)\s+|\bfor\s+[\p{L}]+(?:\s+[\p{L}]+)*\s+)[\s(["'‘’“”]*(?:(?:the|a|an|your|our|their|his|her|its|my)\s+)?(?:minimum|maximum)\s+[\p{L}][\p{L}\p{N}-]*(?:\s+[\p{L}][\p{L}\p{N}-]*)*\s+(?:is|are|was|were|will\s+be)\s*$`)
var estimatedNominalRE = regexp.MustCompile(`(?i)\b(?:estimated|minimum|maximum)\s+[\p{L}][\p{L}\p{N}-]*(?:\s+[\p{L}][\p{L}\p{N}-]*)*\s+of\s*$`)
var estimatedLabelRE = regexp.MustCompile(`(?i)(?:^|,|\b(?:and|but|while)\s+|\bfor\s+[\p{L}]+(?:\s+[\p{L}]+)*\s+)[\s(["'‘’“”]*(?:(?:the|a|an|your|our|their|his|her|its|my)\s+)?(?:estimated|minimum|maximum)\s+[\p{L}][\p{L}\p{N}-]*(?:\s+[\p{L}][\p{L}\p{N}-]*)*\s*:\s*$`)
var estimatedSubjectRE = regexp.MustCompile(`(?i)(?:^|,|\b(?:and|but|while)\s+|\bfor\s+[\p{L}]+(?:\s+[\p{L}]+)*\s+)[\s(["'‘’“”]*(?:(?:the|a|an|your|our|their|his|her|its|my)\s+)?estimated\s+([\p{L}][\p{L}\p{N}-]*(?:\s+[\p{L}][\p{L}\p{N}-]*)*)\s+(?:is|are|was|were|will\s+be)\s*$`)

var independentNominalRE = regexp.MustCompile(`(?i)\b(?:changed|applies|increased|decreased|expired|ended|is|are|was|were)\s+and\b|\band\s+(?:the|a|an)\s+`)

func nominalSubjectMatches(re *regexp.Regexp, s string) bool {
	match := re.FindString(s)
	seen := false
	for word := range strings.FieldsSeq(strings.ToLower(match)) {
		if seen && (slices.Contains([]string{"but", "while", "or"}, word) || word == andWord && independentNominalRE.MatchString(match)) {
			return false
		}
		seen = seen || word == "estimated" || word == "minimum" || word == "maximum"
	}
	return match != ""
}

func qualifier(prefix string) (string, bool) {
	if nominalSubjectMatches(estimatedLabelRE, prefix) || nominalSubjectMatches(estimatedNominalRE, prefix) {
		return "", false
	}
	p, q := splitQualifier(strings.ToLower(strings.Join(strings.Fields(prefix), " ")))
	subject := p
	for {
		loc := dateRE.FindStringIndex(subject)
		if loc == nil {
			break
		}
		before := trimDateOpening(approximationPrefixRE.ReplaceAllString(strings.TrimSpace(subject[:loc[0]]), ""))
		if !endsWord(before, periodWords...) && !endsWord(before, "on") {
			break
		}
		{
			if strings.Contains(before, "estimated ") || strings.Contains(before, "minimum ") || strings.Contains(before, "maximum ") {
				for _, word := range append(slices.Clone(periodWords), "on") {
					if endsWord(before, word) {
						before = strings.TrimSpace(strings.TrimSuffix(before, word))
						break
					}
				}
				subject = before + " " + strings.TrimLeft(subject[loc[1]:], " ,;:)]}\"'”’»")
			} else {
				subject = strings.TrimLeft(subject[loc[1]:], " ,;:)]}\"'”’»")
			}
		}
	}
	if nominalSubjectMatches(nominalBoundSubjectRE, subject) || nominalSubjectMatches(estimatedNominalRE, subject) || nominalSubjectMatches(estimatedLabelRE, subject) || strings.HasSuffix(strings.TrimSpace(prefix), ":") && nominalSubjectMatches(estimatedLabelRE, subject+":") {
		return q, false
	}
	if match := estimatedSubjectRE.FindStringSubmatch(subject); match != nil {
		words := strings.Fields(match[1])
		for i, word := range words {
			if word == andWord && (i == 0 || independentNominalRE.MatchString(match[0]) || figureAssertionRE.MatchString(match[1])) || word == "but" || word == "while" || word == "or" || word == "be" && (i < 2 || words[i-1] != "to") {
				return q, !approximationPrefixRE.MatchString(p)
			}
		}
		return q, false
	}
	return q, !approximationPrefixRE.MatchString(p)
}

// splitQualifier keeps range context visible before a bound such as "at most".
func splitQualifier(p string) (string, string) {
	p = strings.TrimRightFunc(p, func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune(`:"'‘’“”«»`, r) })
	for _, q := range []struct{ ascii, value string }{{"<=", "≤"}, {">=", "≥"}} {
		if trimmed, ok := strings.CutSuffix(p, q.ascii); ok {
			return strings.TrimSpace(trimmed), q.value
		}
	}
	for _, symbol := range []string{"≤", "≥", ">", "<"} {
		if trimmed, ok := strings.CutSuffix(p, symbol); ok {
			return strings.TrimSpace(trimmed), symbol
		}
	}
	boundPrefix := p
	for _, article := range []string{"a", "an", "the"} {
		if endsWord(boundPrefix, article) {
			boundPrefix = strings.TrimSpace(strings.TrimSuffix(boundPrefix, article))
			break
		}
	}
	for _, q := range boundWords {
		for _, word := range q.words {
			if endsWord(strings.ReplaceAll(boundPrefix, "(", " "), word) {
				prefix := strings.TrimSpace(strings.TrimSuffix(boundPrefix, word))
				if strings.HasPrefix(word, "minimum") || strings.HasPrefix(word, "maximum") {
					for _, article := range []string{"a", "the"} {
						if endsWord(prefix, article) {
							prefix = strings.TrimSpace(strings.TrimSuffix(prefix, article))
							break
						}
					}
				}
				return prefix, q.value
			}
		}
	}
	return p, ""
}
func splitClauses(s string) []clause {
	var out []clause
	start := 0
	partial := false
	emit := func(end int, nextPartial bool) {
		a, b := start, end
		for a < b {
			r, n := utf8.DecodeRuneInString(s[a:b])
			if !unicode.IsSpace(r) {
				break
			}
			a += n
		}
		for b > a {
			r, n := utf8.DecodeLastRuneInString(s[a:b])
			if !unicode.IsSpace(r) {
				break
			}
			b -= n
		}
		if a < b {
			out = append(out, clause{a, b, partial || nextPartial})
			partial = false
		}
		partial = partial || nextPartial
	}
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		if strings.HasPrefix(s[i:], "...") {
			emit(i, true)
			for i < len(s) && s[i] == '.' {
				i++
			}
			start = i
			continue
		}
		if r == '…' {
			emit(i, true)
			i += n
			start = i
			continue
		}
		if r == ';' || r == '\n' && hardLineBreak(s, i) {
			emit(i, false)
			i += n
			start = i
			continue
		}
		if r == '.' || r == '!' || r == '?' {
			next := i + n
			for next < len(s) {
				closer, size := utf8.DecodeRuneInString(s[next:])
				if !strings.ContainsRune(closingPunctuation, closer) {
					break
				}
				next += size
			}
			nextRune, _ := utf8.DecodeRuneInString(s[next:])
			if next == len(s) || unicode.IsSpace(nextRune) {
				prefix := strings.ToLower(norm.NFKC.String(s[start : i+n]))
				abbrev := normalizedDurationContinuation(prefix, norm.NFKC.String(s[next:])) || abbreviationRE.MatchString(prefix) || noAbbreviationRE.MatchString(prefix) && identifierAbbreviation(s[start:], i-start) || maxAbbreviationRE.MatchString(prefix) && figureLeadRE.MatchString(s[next:]) && !endsTypedFigure(strings.TrimSpace(prefix[:len(prefix)-4]))
				if !abbrev {
					emit(next, false)
					start = next
				}
			}
		}
		i += n
	}
	emit(len(s), false)
	return out
}

// Blank lines and list or table rows separate independent statements.
var capitalArticleRE = regexp.MustCompile(`^\s+(?:(?:A|An|Each)\s|P(?i:er\s+(?:[\p{L}]+(?:\s+[\p{L}]+)*\s*,|[\p{L}]+\s+(?:the|a|an)\s+[\p{L}]+(?:\s+[\p{L}]+)*\s+(?:is|are|was|were|has|have)\b|[\p{L}]+\s+[\p{L}]+\s+(?:start|starts|begin|begins|apply|applies)\s+from\b|[\p{L}]+(?:\s+[\p{L}]+)*\s+(?:` + negationRE.String() + `|[\p{L}]+n['’]t)\b)))`)

var independentExclusionRE = regexp.MustCompile(`^\s*Excluding\s+[\p{L}]+(?:\s+[\p{L}]+)*\s*,\s*(?:the|a|an)\s+[\p{L}]+`)

var independentApproxLeadRE = regexp.MustCompile(`^\s*(?:About|Approximately|Around|Roughly|Nearly|Almost|Circa|Estimated)\b`)

func independentApproxAssertion(s string) bool {
	loc := figureAssertionRE.FindStringIndex(s)
	return independentApproxLeadRE.MatchString(s) && loc != nil && quantitySubjectRE.MatchString(s[loc[1]:])
}

var figureAssertionRE = regexp.MustCompile(moneyRE.String() + `|` + unitRE.String() + `|` + countRE.String())

var independentBoundSentenceRE = regexp.MustCompile(`(?i)^\s*(?:maximum|minimum)\s+[\p{L}]+(?:\s+[\p{L}]+)*\s+(?:has|have|is|are|was|were)\s+[\p{L}]+\b`)
var quantifiedAssertionPrefixRE = regexp.MustCompile(`(?i)^\s*(?:` + rateBoundPrefix + `)\s*$`)

func independentBoundAssertion(s string) bool {
	loc := figureAssertionRE.FindStringIndex(s)
	return independentBoundSentenceRE.MatchString(s) || loc != nil && (nominalSubjectMatches(nominalBoundSubjectRE, s[:loc[0]]) || nominalSubjectMatches(estimatedLabelRE, s[:loc[0]]) || quantifiedAssertionPrefixRE.MatchString(s[:loc[0]]) && quantitySubjectRE.MatchString(s[loc[1]:]))
}

func normalizedDurationContinuation(prefix, suffix string) bool {
	return durationAbbrevRE.MatchString(prefix) && (approximationSuffixRE.MatchString(suffix) && !independentApproxAssertion(suffix) || durationAlternativeRE.MatchString(suffix) || uncertaintySuffixRE.MatchString(suffix) || durationArithmeticRE.MatchString(suffix) || rateEnd(suffix) > 0 && !capitalArticleRE.MatchString(suffix) || calendarExclusionRE.MatchString(suffix) && !independentExclusionRE.MatchString(suffix) || postfixBound(suffix) && !independentBoundAssertion(suffix) || rateDateContinuationRE.MatchString(suffix))
}

var durationAbbrevRE = regexp.MustCompile(`(?i)(?:\b|[0-9])(?:mins?|hrs?)\.$`)
var rateDateContinuationRE = regexp.MustCompile(`^\s+(?:` + strings.ReplaceAll(strings.Join(periodWords, "|"), " ", `\s+`) + `)\s+` + dateRE.String() + `(?:\s+(?:until|till|through|to)\s+` + dateRE.String() + `)?\s*[.!?](?:\s|$)`)
var durationArithmeticRE = regexp.MustCompile(`(?i)^\s*(?:[+−*/⁄×÷±∓^\p{Pd}⁓⁻₋<>=≤≥]|out\s+of\b|(?:multiplied\s+by|divided\s+by|times|plus|minus|x)\b|and\s+(?:a\s+few|few|some|several)\s+|and\s+(?:a|an)\s+(?:` + approximation + `\s+` + number + `\b|(?:hours?|hrs?|minutes?|mins?|days?|weeks?|months?|years?)\s*[.,;!?]|(?:hours?|hrs?|minutes?|mins?|days?|weeks?|months?|years?)\s*$)|and\s+(?:a\s+)?` + fractionWords + `s?\b|[(\[]\s*(?:[+−*/⁄×÷±∓^-]|(?:and|plus|minus|times|x|multiplied|divided)\b))`)
var durationAlternativeRE = regexp.MustCompile(`(?i)^\s+(?:or|and|through|to|until|till)\s+[(\["'‘’“”«»]*\s*(?:(?:` + approximation + `|` + rateBoundPrefix + `)\s+|(?:<=|>=|≤|≥|<|>)\s*)?(?:[$€£¥]|(?:` + currencyCodes + `)\s*)?(?:` + number + `)\b`)
var listRowRE = regexp.MustCompile(`^\s*(?:[-*+]\s+|[0-9]+[.)]\s+)`)

func hardLineBreak(s string, i int) bool {
	start := strings.LastIndexByte(s[:i], '\n') + 1
	end := strings.IndexByte(s[i+1:], '\n')
	if end < 0 {
		end = len(s)
	} else {
		end += i + 1
	}
	before, after := s[start:i], s[i+1:end]
	return strings.TrimSpace(before) == "" || strings.TrimSpace(after) == "" ||
		strings.Contains(before, "|") || strings.Contains(after, "|") ||
		listRowRE.MatchString(before) && strings.TrimLeft(after, " \t") == after || listRowRE.MatchString(after)
}

var abbreviationRE = regexp.MustCompile(`(?i)(?:^|[^\pL\pN_])(?:e\.g\.|i\.e\.|mr\.|mrs\.|dr\.)$`)
var noAbbreviationRE = regexp.MustCompile(`(?:^|[^\pL\pN_])no\.$`)
var maxAbbreviationRE = regexp.MustCompile(`(?:^|[^\pL\pN_])max\.$`)
var figureLeadRE = regexp.MustCompile(`(?i)^\s*(?:[$€£¥]|` + currencyCodes + `\b|` + numberPattern() + `\b)`)

// Identifier labels own No.; a standalone answer ends its sentence.
func identifierAbbreviation(s string, dot int) bool {
	return slices.ContainsFunc(identifierExplicitTokenRE.FindAllStringIndex(s, -1), func(loc []int) bool {
		return loc[0] <= dot && dot < loc[1]
	})
}

var numberWords = map[string]string{"zero": "0", "one": "1", "two": "2", "three": "3", "four": "4", "five": "5", "six": "6", "seven": "7", "eight": "8", "nine": "9", "ten": "10", "eleven": "11", "twelve": "12", "thirteen": "13", "fourteen": "14", "fifteen": "15", "sixteen": "16", "seventeen": "17", "eighteen": "18", "nineteen": "19", "twenty": "20"}

func numberPattern() string {
	words := make([]string, 0, len(numberWords))
	for word := range numberWords {
		words = append(words, word)
	}
	sort.Slice(words, func(i, j int) bool {
		if len(words[i]) == len(words[j]) {
			return words[i] < words[j]
		}
		return len(words[i]) > len(words[j])
	})
	return `(?:[0-9]+(?:,[0-9]{3})*(?:\.[0-9]+)?|` + strings.Join(words, "|") + `)`
}

const numericOperators = "−+*/⁄×÷±∓^"

func numericOperator(r rune) bool {
	return unicode.Is(unicode.Dash, r) || strings.ContainsRune(numericOperators, r)
}

// Do not extract a supported suffix from an unsupported compound numeral.
func compoundNumberPrefix(prefix string) bool {
	prefix = listRowRE.ReplaceAllString(prefix, "")
	if groupedNumberPrefix(strings.TrimRightFunc(prefix, unicode.IsSpace)) {
		return true
	}
	prefix = strings.TrimRightFunc(prefix, func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune(`("'‘’“”«»`, r) })
	prefix, _ = splitQualifier(strings.ToLower(strings.Join(strings.Fields(prefix), " ")))
	prefix = strings.TrimRightFunc(prefix, func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune(`(["'‘’“”«»`, r) })
	prefix = strings.TrimRightFunc(strings.TrimSpace(arithmeticByRE.ReplaceAllString(prefix, "")), func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune(`)]"'‘’“”«»`, r) })
	for _, article := range []string{"the", "a", "an", "every"} {
		if endsWord(prefix, "of "+article) || endsWord(prefix, "in "+article) {
			prefix = strings.TrimSpace(strings.TrimSuffix(prefix, article))
			break
		}
	}
	if endsWord(prefix, "in", "of") && !endsWord(prefix, "out of") {
		i := strings.LastIndex(prefix, " ")
		if i >= 0 && compoundNumberPrefix(strings.TrimSpace(prefix[:i])) {
			return true
		}
	}
	if endsWord(prefix, "out of") && !endsTypedFigure(strings.TrimSuffix(strings.TrimSpace(strings.TrimSuffix(prefix, "out of")), ".")) {
		prefix = strings.TrimSpace(strings.TrimSuffix(prefix, "out of"))
	}
	r, _ := utf8.DecodeLastRuneInString(prefix)
	if unicode.IsDigit(r) || numericOperator(r) || groupedNumberPrefix(prefix) || endsWord(prefix, "out of") && endsTypedFigure(strings.TrimSuffix(strings.TrimSpace(strings.TrimSuffix(prefix, "out of")), ".")) || endsWord(prefix, "between", "minus", "negative", "plus", "less", "twice") || fractionPrefixRE.MatchString(prefix) {
		return true
	}
	i := strings.LastIndexFunc(prefix, func(r rune) bool { return !unicode.IsLetter(r) })
	connector := strings.ToLower(prefix[i+1:])
	if connector == andWord || connector == "or" || connector == "point" || connector == "to" || connector == "through" || connector == "until" || connector == "till" || connector == "x" || connector == "times" {
		prefix = strings.TrimRightFunc(prefix[:i+1], func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune(`)]"'‘’“”«»`, r) })
		if connector == andWord && endsTypedFigure(prefix) {
			return false
		}
		r, _ = utf8.DecodeLastRuneInString(prefix)
		if unicode.IsDigit(r) {
			return true
		}
		i = strings.LastIndexFunc(prefix, func(r rune) bool { return !unicode.IsLetter(r) })
	}
	return numeralWord(prefix[i+1:])
}

func rateContains(s string, checks []candidate, c candidate) bool {
	return slices.ContainsFunc(checks, func(rate candidate) bool {
		return rate.priority > 0 && rate.end <= c.start && c.end <= rate.end+rateEnd(s[rate.end:])
	})
}

func percentageBaseConnector(s string) bool {
	between := strings.ToLower(strings.Join(strings.Fields(s), " "))
	between = strings.Trim(approximationSuffixRE.ReplaceAllString(between, ""), " )]\"'‘’“”»([«")
	between, _ = splitQualifier(approximationPrefixRE.ReplaceAllString(between, ""))
	between = strings.Trim(between, " )]\"'‘’“”»([«")
	return slices.Contains([]string{"of", "of the", "of a", "of an", "on", "on the", "on a", "on an"}, between)
}

func percentageBaseEnd(s string, checks []candidate, rate candidate) int {
	if rate.f.Kind != percentKind || rate.f.Unit != percentKind {
		return rate.end
	}
	for _, c := range checks {
		if c.priority == 0 || c.start < rate.end {
			continue
		}
		if percentageBaseConnector(s[rate.end:c.start]) {
			return percentageBaseGroupEnd(s, checks, c)
		}
	}
	return rate.end
}

func percentageBaseContains(s string, checks []candidate, c candidate) bool {
	return slices.ContainsFunc(checks, func(rate candidate) bool {
		return rate.end <= c.start && c.end <= percentageBaseEnd(s, checks, rate) && rate.end < c.end
	})
}

func figureRelationEnd(s string, checks []candidate, c candidate, limit int) int {
	end := percentageBaseEnd(s, checks, c)
	if end > limit {
		end = c.end
	}
	return max(end, c.end+rateEnd(s[c.end:limit]))
}

// Approximation scopes coordinated figures; bounds and exclusions scope duration groups.
func omitUncertainGroups(s string, checks, cs []candidate) []candidate {
	var figures []candidate
	for _, c := range checks {
		if (c.f.Kind == dateKind || c.f.Kind == durationKind || c.f.Kind == "money" || c.f.Kind == percentKind || c.f.Kind == "count") && !rateContains(s, checks, c) && !percentageBaseContains(s, checks, c) {
			figures = append(figures, c)
		}
	}
	sort.SliceStable(figures, func(i, j int) bool { return figures[i].start < figures[j].start })
	previousExcluded := false
	for i := 0; i < len(figures); {
		j := i + 1
		for j < len(figures) && figures[j].f.Kind == figures[i].f.Kind && figures[j-1].end <= figures[j].start && !quantitySubjectRE.MatchString(s[figures[j].end:]) && durationJoined(s[figureRelationEnd(s, checks, figures[j-1], figures[j].start):figures[j].start]) {
			j++
		}
		uncertain, excluded := false, previousExcluded && i > 0 && figures[i-1].end <= figures[i].start && !quantitySubjectRE.MatchString(s[figures[i].end:]) && durationJoinRE.MatchString(s[figures[i-1].end:figures[i].start])
		if loc := numericExclusionRE.FindStringIndex(s[:figures[i].start]); loc != nil {
			words := strings.FieldsFunc(strings.ToLower(s[loc[0]:figures[i].start]), func(r rune) bool { return !unicode.IsLetter(r) })
			excluded = excluded || !slices.Contains(words, andWord) && !slices.Contains(words, "or") && !slices.Contains(words, "but") && !slices.Contains(words, "while")
		}
		for _, c := range figures[i:j] {
			q, exact := qualifier(s[:c.start])
			end := c.end + rateEnd(s[c.end:])
			uncertain = uncertain || !exact || approximationSuffixRE.MatchString(s[end:]) || uncertaintyAdjacent(s[:c.start], s[end:])
			if c.f.Kind == durationKind {
				uncertain = uncertain || q != "" || postfixFigureBound(c.f, s[c.end:])
				excluded = excluded || calendarExclusionRE.MatchString(s[c.end:])
			}
		}
		if excluded || uncertain && j-i > 1 {
			cs = slices.DeleteFunc(cs, func(c candidate) bool {
				return c.start >= figures[i].start && c.end <= figures[j-1].end+rateEnd(s[figures[j-1].end:])
			})
		}
		previousExcluded = excluded
		i = j
	}
	return cs
}

var durationArticleRE = regexp.MustCompile(`(?i)\b(?:a|an)\s+(` + approximation + `)\b`)
var durationJoinRE = regexp.MustCompile(`(?i)^[\s.)\]"'‘’“”«»]*(?:,\s*(?:and\s+)?|and\s+)[\s(\["'‘’“”«»]*$`)

func durationJoined(s string) bool {
	return durationJoinRE.MatchString(approximationPrefixRE.ReplaceAllString(durationArticleRE.ReplaceAllString(durationJoinTail(s), " "), " "))
}

func durationJoinTail(s string) string {
	if loc := exclusionJoinRE.FindStringSubmatchIndex(s); loc != nil {
		s = s[loc[2]:]
	} else if loc := approximationSuffixRE.FindStringIndex(s); loc != nil {
		s = s[loc[1]:]
	} else if end := postfixBoundEnd(s); end > 0 {
		s = s[end:]
	}
	return s
}

// A grouping separator between digits is not a scalar boundary.
func groupedNumberPrefix(prefix string) bool {
	r, size := utf8.DecodeLastRuneInString(prefix)
	if !strings.ContainsRune("_'’‘ʼ", r) {
		return false
	}
	r, _ = utf8.DecodeLastRuneInString(prefix[:len(prefix)-size])
	return unicode.IsDigit(r)
}

func groupedNumberSuffix(suffix string) bool {
	r, size := utf8.DecodeRuneInString(suffix)
	if !strings.ContainsRune("_'’‘ʼ", r) {
		return false
	}
	r, _ = utf8.DecodeRuneInString(suffix[size:])
	return unicode.IsDigit(r)
}

var qualifiedTailRE = regexp.MustCompile(`(?i)\s+(?:(?:` + rateBoundPrefix + `)|<=|>=|≤|≥|<|>)\s*$`)

func stripQualifiedTail(s string) string {
	loc := qualifiedTailRE.FindStringIndex(s)
	if loc != nil {
		return s[:loc[0]]
	}
	return s
}

// An accepted date immediately before a figure is not a compound numeral.
func compoundPrefixAfterDates(prefix string, cs []candidate) bool {
	trimmed := strings.TrimRightFunc(stripQualifiedTail(prefix), unicode.IsSpace)
	if len(trimmed) < len(prefix) {
		before := strings.TrimRight(trimmed, ",:)]}\"'”’»")
		if len(before) < len(trimmed) && slices.ContainsFunc(cs, func(c candidate) bool { return c.f.Kind == "identifier" && c.end == len(before) }) {
			return false
		}
	}
	if rest, ok := strings.CutSuffix(trimmed, ":"); ok && len(trimmed) < len(prefix) {
		before := strings.TrimRightFunc(rest, unicode.IsSpace)
		if slices.ContainsFunc(cs, func(c candidate) bool { return c.f.Kind == dateKind && c.end == len(before) }) {
			return false
		}
	}
	if rest, ok := strings.CutSuffix(trimmed, ","); ok {
		before := strings.TrimRightFunc(rest, unicode.IsSpace)
		r, _ := utf8.DecodeLastRuneInString(before)
		if unicode.IsDigit(r) {
			return !slices.ContainsFunc(cs, func(c candidate) bool { return c.end == len(before) }) && !identifierDetachedBoundary(trimmed)
		}
	}
	if len(trimmed) < len(prefix) && slices.ContainsFunc(cs, func(c candidate) bool {
		return c.f.Kind == dateKind && c.end == len(trimmed)
	}) {
		return false
	}
	return compoundNumberPrefix(prefix) && !acceptedDateConjunction(prefix, cs) && !acceptedIdentifierConjunction(prefix, cs) && !continuedIdentifierConjunction(prefix)
}

func acceptedDateConjunction(prefix string, cs []candidate) bool {
	return slices.ContainsFunc(cs, func(c candidate) bool {
		return c.f.Kind == dateKind && c.end <= len(prefix) && strings.EqualFold(strings.TrimSpace(prefix[c.end:]), "and")
	})
}

func identifierDetachedBoundary(prefix string) bool {
	return slices.ContainsFunc(identifierRE.FindAllStringIndex(prefix, -1), func(span []int) bool { return identifierTokenEnd(prefix, span[1]) == len(prefix) })
}

func continuedIdentifierConjunction(prefix string) bool {
	return slices.ContainsFunc(identifierRE.FindAllStringIndex(prefix, -1), func(span []int) bool {
		suffix := stripQualifiedTail(prefix[identifierTokenEnd(prefix, span[1]):])
		return identifierConjunctionRE.MatchString(suffix) && slices.Contains([]string{andWord, "but", "while"}, strings.ToLower(strings.TrimSpace(suffix)))
	})
}

func acceptedIdentifierConjunction(prefix string, cs []candidate) bool {
	return slices.ContainsFunc(cs, func(c candidate) bool {
		return c.f.Kind == "identifier" && c.end <= len(prefix) && strings.EqualFold(strings.TrimSpace(stripQualifiedTail(prefix[c.end:])), "and")
	})
}

func boundedCompoundDurationRate(s string) bool {
	if !rateBoundLeadRE.MatchString(s) {
		return false
	}
	matches := unitRE.FindAllStringSubmatchIndex(s, 2)
	if len(matches) != 2 || !durationJoined(s[matches[0][1]:matches[1][0]]) {
		return false
	}
	first, ok := parseUnit(s[matches[0][0]:matches[0][1]], unitRE.FindStringSubmatch(s[matches[0][0]:matches[0][1]]))
	if !ok || first.Kind != durationKind {
		return false
	}
	second, ok := parseUnit(s[matches[1][0]:matches[1][1]], unitRE.FindStringSubmatch(s[matches[1][0]:matches[1][1]]))
	return ok && second.Kind == durationKind
}

// A rejected numerator or postfix qualifier applies to the complete rate.
func omitUncertainRates(s string, checks, cs []candidate) []candidate {
	for _, c := range checks {
		if c.priority == 0 {
			continue
		}
		n := rateEnd(s[c.end:])
		accepted := slices.ContainsFunc(cs, func(other candidate) bool { return other.start == c.start && other.end == c.end })
		uncertainDenominator := rateApproximationRE.MatchString(s[c.end:]) || boundedRateRange(s[c.end:]) || boundedCompoundDurationRate(s[c.end:c.end+n]) || slices.ContainsFunc(checks, func(part candidate) bool {
			_, exact := qualifier(s[:part.start])
			return (part.f.Kind == durationKind || part.f.Kind == "count" || part.f.Kind == percentKind) && part.start >= c.end && part.end <= c.end+n &&
				(!exact || !slices.ContainsFunc(cs, func(accepted candidate) bool { return accepted.start == part.start && accepted.end == part.end }) || approximationSuffixRE.MatchString(s[part.end:]) || postfixFigureBound(part.f, s[part.end:]) || uncertaintyAdjacent(s[:part.start], s[part.end:]) || calendarExclusionRE.MatchString(s[part.end:]))
		})
		if n == 0 && !uncertainDenominator || accepted && !uncertainDenominator && !postfixFigureBound(c.f, s[c.end+n:]) && !approximationSuffixRE.MatchString(s[c.end+n:]) && !uncertaintyAdjacent(s[:c.start], s[c.end+n:]) && !parenthesizedMoney(c.f, s[:c.start], s[c.end:]) {
			continue
		}
		cs = slices.DeleteFunc(cs, func(other candidate) bool {
			return other.start >= c.start && other.end <= c.end+n
		})
	}
	return cs
}

func extendPercentageBases(s string, checks, bases []candidate) []candidate {
	for i := 0; i < len(bases); i++ {
		base := bases[i]
		for _, c := range checks {
			if c.f.Kind == base.f.Kind && c.start >= base.end && durationJoined(s[base.end:c.start]) && !quantitySubjectRE.MatchString(s[c.end:]) {
				bases = append(bases, c)
				break
			}
		}
	}
	return bases
}

func percentageBaseGroupEnd(s string, checks []candidate, first candidate) int {
	bases := extendPercentageBases(s, checks, []candidate{first})
	return bases[len(bases)-1].end
}

// A percentage's base is an operand, not a separate asserted figure.
func omitPercentageBases(s string, checks, cs []candidate) []candidate {
	var bases []candidate
	for _, rate := range checks {
		if rate.f.Kind != percentKind || rate.f.Unit != percentKind {
			continue
		}
		for _, c := range checks {
			if c.priority == 0 || c.start < rate.end {
				continue
			}
			if percentageBaseConnector(s[rate.end:c.start]) {
				bases = append(bases, c)
				break
			}
		}
	}
	bases = extendPercentageBases(s, checks, bases)
	return slices.DeleteFunc(cs, func(c candidate) bool {
		return slices.ContainsFunc(bases, func(base candidate) bool { return c.start >= base.start && c.end <= base.end+rateEnd(s[base.end:]) })
	})
}

// A numeric denominator remains a figure for later rate classification.
func compoundPrefixWithRates(s string, start int, checks, cs []candidate) bool {
	for _, c := range checks {
		if c.priority > 0 && c.end <= start && strings.HasPrefix(strings.TrimSpace(s[c.end:]), "/") {
			if n := rateEnd(s[c.end:]); n > 0 && start < c.end+n {
				return false
			}
		}
	}
	return compoundPrefixAfterDates(s[:start], cs)
}

// Validate relationships only after all recognizers have collected candidates.
func unsafeCandidates(s string, checks, cs []candidate) bool {
	for _, left := range checks {
		for _, right := range checks {
			if left.priority > 0 && right.priority > 0 && left.f.Kind == right.f.Kind && left.end < right.start && alternativePeriodRE.MatchString(durationJoinTail(s[left.end:right.start])) {
				return true
			}
			if left.priority > 0 && right.priority > 0 && left.end < right.start && comparativeRE.MatchString(s[left.end:right.start]) {
				return true
			}
			if left.f.Kind == "money" && right.f.Kind == "money" && left.end < right.start && strings.TrimSpace(s[left.end:right.start]) == "," && commaMoneyGroupRE.MatchString(s[right.start:right.end]) {
				return true
			}
		}
	}
	return slices.ContainsFunc(checks, func(c candidate) bool {
		return c.priority > 0 && (bareComparisonRE.MatchString(s[c.end:]) || compoundPrefixWithRates(s, c.start, checks, cs) || compoundSuffixBeforeDates(s, c.end, c.f, cs, checks))
	})
}

var bareComparisonRE = regexp.MustCompile(`(?i)^[\s.()\[\]"'‘’“”«»]*(?:<=|>=|<|>|≤|≥|(?:less|more|greater|higher|lower)\s+than(?:\s+or\s+equal\s+to)?)\s*[(\["'‘’“”«]*\s*(?:` + number + `)\b`)

var comparativeRE = regexp.MustCompile(`(?i)^[\s()\[\]"'‘’“”«»]*(?:\s*(?:<=|>=|<|>|≤|≥)\s*|\s+(?:(?:less|more|greater|higher|lower)\s+than(?:\s+or\s+equal\s+to)?|up\s+to|after|before|prior\s+to)\s+)[\s(["'‘’“”«»]*$`)

var commaMoneyGroupRE = regexp.MustCompile(`^[0-9]{3}(?:,[0-9]{3})*(?:\.[0-9]+)?(?:\s|[a-zA-Z$€£¥]|$)`)

// A rate requires a letter-bearing noun, not a bare numeric divisor.
func unsupportedOriginalRate(original string, offsets map[int]int, s string, end int) bool {
	loc := rateRE.FindStringSubmatchIndex(s[end:])
	if loc == nil || loc[2] < 0 {
		return false
	}
	start, okStart := offsets[end+loc[2]]
	finish, okEnd := offsets[end+loc[3]]
	return okStart && okEnd && strings.ContainsAny(original[start:finish], unsupportedNumericDigits)
}

var rateApproximationRE = regexp.MustCompile(`(?i)^\.?(?:\s*/\s*|\s+(?:per|each|an|a)\s+)[(\["'‘’“”\s]*(?:` + approximation + `\b|[~≈])`)

var rateBoundPrefix = func() string {
	var words []string
	for _, bound := range boundWords {
		for _, word := range bound.words {
			words = append(words, strings.ReplaceAll(regexp.QuoteMeta(word), " ", `\s+`))
		}
	}
	return strings.Join(words, "|")
}()

var rateBoundLeadRE = regexp.MustCompile(`(?i)^\.?(?:\s*/\s*|\s+(?:per|each|an|a)\s+)[(\["'‘’“”\s]*(?:(?:` + rateBoundPrefix + `)\s+|(?:<=|>=|≤|≥|<|>)\s*)`)

var rateFractionRE = regexp.MustCompile(`(?i)^\.?(?:\s*/\s*|\s+(?:per|each|an|a)\s+)[(\["'‘’“”\s]*(?:` + number + `\s+)?` + fractionWords + `s?\b`)

var rateRE = regexp.MustCompile(`(?i)^\.?(?:\s*/\s*|\s+(?:per|each|an|a)\s+)[(\["'‘’“”]?\s*(?:(?:` + approximation + `(?:\s+(?:exactly|a\s+total\s+of|to\s+be|equal\s+to|at|a|the))?\s+|[~≈]\s*)[(\["'‘’“”]?\s*)?(?:(?:` + rateBoundPrefix + `)\s+|(?:<=|>=|≤|≥|<|>)\s*)?(?:((?:` + numRange + `))\s+)?(?:(?:business|working|calendar|free|paid|additional|extra)\s+)?(percentage\s+points?|per\s+cent|[\p{L}][\p{L}\p{Nd}]*)`)

func boundedRateRange(s string) bool {
	loc := rateRE.FindStringSubmatchIndex(s)
	if loc == nil || loc[2] < 0 || !rateBoundLeadRE.MatchString(s) {
		return false
	}
	_, end, ok := numbers(s[loc[2]:loc[3]], 1)
	return ok && end != ""
}

func rateEnd(s string) int {
	loc := rateRE.FindStringSubmatchIndex(s)
	if loc == nil || numeralWord(s[loc[4]:loc[5]]) || loc[2] < 0 && rateBoundLeadRE.MatchString(s) || rateFractionRE.MatchString(s) {
		return 0
	}
	if loc[2] >= 0 {
		if _, _, ok := numbers(s[loc[2]:loc[3]], 1); !ok {
			return 0
		}
	}
	if loc[1] < len(s) {
		r, _ := utf8.DecodeRuneInString(s[loc[1]:])
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			return 0
		}
	}
	end := loc[1]
	for _, re := range []*regexp.Regexp{unitRE, countRE} {
		first := re.FindStringSubmatchIndex(s[:end])
		if first == nil || first[1] != end {
			continue
		}
		part := s[first[0]:first[1]]
		var f Figure
		var ok bool
		if re == unitRE {
			f, ok = parseUnit(part, re.FindStringSubmatch(part))
		} else {
			f, ok = parseCount(part, re.FindStringSubmatch(part))
		}
		if !ok || f.Kind != durationKind && f.Kind != "count" && f.Kind != percentKind {
			continue
		}
		kind := f.Kind
		for {
			next := re.FindStringIndex(s[end:])
			if next == nil || !durationJoined(s[end:end+next[0]]) || quantitySubjectRE.MatchString(s[end+next[1]:]) {
				break
			}
			part = s[end+next[0] : end+next[1]]
			if re == unitRE {
				f, ok = parseUnit(part, re.FindStringSubmatch(part))
				if !ok || f.Kind != kind {
					break
				}
			}
			end += next[1]
		}
		break
	}
	if strings.HasPrefix(s[end:], ".") && durationAbbrevRE.MatchString(s[:end+1]) {
		end++
	}
	return end
}

// A stated end date is not an unsupported numeric range endpoint.
func compoundSuffixBeforeDates(s string, end int, f Figure, cs, checks []candidate) bool {
	if f.Kind == durationKind && strings.HasPrefix(s[end:], ".") && durationAbbrevRE.MatchString(s[:end+1]) {
		end++
	}
	if slices.ContainsFunc(checks, func(c candidate) bool {
		return dateBoundUsable(s, cs, c) && c.start >= end && (plainDateBoundConnectorRE.MatchString(s[end:c.start]) || rateBoundConnector(s[end:c.start]))
	}) {
		return false
	}
	if n := rateEnd(s[end:]); n > 0 {
		if slices.ContainsFunc(checks, func(c candidate) bool {
			return c.f.Kind == dateKind && c.start >= end && c.start < end+n && c.end > end+n
		}) {
			return false
		}
		rateEnd := end + n
		if strings.HasPrefix(s[rateEnd:], ".") && durationAbbrevRE.MatchString(s[end:rateEnd+1]) {
			rateEnd++
		}
		return bareComparisonRE.MatchString(s[rateEnd:]) || compoundNumberSuffix(s[rateEnd:], f)
	}
	return compoundNumberSuffix(s[end:], f)
}

func dateBoundUsable(s string, cs []candidate, c candidate) bool {
	return c.f.Kind == dateKind && (slices.ContainsFunc(cs, func(accepted candidate) bool { return accepted.start == c.start && accepted.end == c.end }) || c.f.Value != "" && incompleteAlternativeDateSuffix(s[c.end:]))
}

func rateBoundConnector(s string) bool {
	n := rateEnd(s)
	return n > 0 && plainDateBoundConnectorRE.MatchString(s[n:])
}

var plainDateBoundConnectorRE = regexp.MustCompile(`(?i)^\s+(?:until|till|through|to)\s+[(\["'‘’“”«]*\s*$`)
var dateBoundConnectorRE = regexp.MustCompile(`(?i)^\s*(?:` + rateRE.String() + `\.?)?\s+(?:until|till|through|to)\s+[(\["'‘’“”«]*\s*$`)

func endsTypedFigure(s string) bool {
	for _, re := range []*regexp.Regexp{moneyRE, unitRE, countRE} {
		matches := re.FindAllStringIndex(s, -1)
		if len(matches) > 0 && matches[len(matches)-1][1] == len(s) {
			return true
		}
	}
	return false
}

func numeralWord(s string) bool {
	s = strings.ToLower(s)
	switch s {
	case "thirty", "forty", "fifty", "sixty", "seventy", "eighty", "ninety", "hundred", "thousand", "million", "billion", "trillion", "bn":
		return true
	}
	_, ok := numberWords[s]
	return ok
}

var arithmeticByRE = regexp.MustCompile(`(?:^|\b)(?:multiplied|divided) by$`)
var arithmeticConnectors = map[string]bool{"multiplied": true, "divided": true, "times": true, "x": true}
var multipliedByRE = regexp.MustCompile(`(?i)^by\b`)
var indefiniteQuantityRE = regexp.MustCompile(`(?i)^(?:a\s+few|a|an|few|some|several)\s+`)
var lessOperandRE = regexp.MustCompile(`(?i)^(?:a|an|the)\s+[\p{L}]+`)
var lessBareOperandRE = regexp.MustCompile(`(?i)^[\p{L}]+`)
var lessArticleRE = regexp.MustCompile(`(?i)^(?:a|an|the)\s+(?:(?:[\p{L}]+\s+){1,3}of\s+)?`)
var typedFigureLeadRE = regexp.MustCompile(`^(?:` + moneyRE.String() + `|` + unitRE.String() + `|` + countRE.String() + `)`)
var currencySubunitRE = regexp.MustCompile(`(?i)^cents?\b`)
var quantitySubjectRE = regexp.MustCompile(`(?i)^\s+(?:is|are|was|were|has|have|had|will|would|can|could|may|might|must|do|does|did)\b`)

var proportionArticleRE = regexp.MustCompile(`(?i)^(?:every|a|an|the)\s+`)
var repeatedNumericSeparatorRE = regexp.MustCompile(`^[,.]{2,}[0-9]`)

func compoundNumberSuffix(suffix string, f Figure) bool {
	if repeatedNumericSeparatorRE.MatchString(suffix) {
		return true
	}
	r, _ := utf8.DecodeRuneInString(suffix)
	if unicode.IsLetter(r) || unicode.IsDigit(r) || groupedNumberSuffix(suffix) {
		return true
	}
	suffix = strings.TrimLeftFunc(suffix, func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune("([‘“"+closingPunctuation, r) })
	r, _ = utf8.DecodeRuneInString(suffix)
	if numericOperator(r) {
		return true
	}
	joined := strings.HasPrefix(suffix, ",")
	suffix = strings.TrimLeftFunc(strings.TrimPrefix(suffix, ","), func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune(`(["'‘’“”«»`, r) })
	r, _ = utf8.DecodeRuneInString(suffix)
	if numericOperator(r) {
		return true
	}
	i := strings.IndexFunc(suffix, func(r rune) bool { return !unicode.IsLetter(r) })
	if i < 0 {
		i = len(suffix)
	}
	connector := strings.ToLower(suffix[:i])
	switch connector {
	case "or", "plus", "minus", "point", "off":
		return true
	case "out":
		tail := strings.TrimLeftFunc(suffix[i:], unicode.IsSpace)
		if len(tail) >= 2 && strings.EqualFold(tail[:2], "of") && (len(tail) == 2 || unicode.IsSpace(rune(tail[2]))) {
			suffix = proportionArticleRE.ReplaceAllString(strings.TrimLeftFunc(tail[2:], unicode.IsSpace), "")
		}
	case "less", "x", "times", "per", "each", "a", "an":
		suffix = strings.TrimLeftFunc(suffix[i:], func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune(`(["'‘’“”«»`, r) })
	case "multiplied", "divided":
		tail := strings.TrimLeftFunc(suffix[i:], unicode.IsSpace)
		if loc := multipliedByRE.FindStringIndex(tail); loc != nil {
			suffix = strings.TrimLeftFunc(tail[loc[1]:], func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune(`(["'‘’“”«»`, r) })
		}
	case andWord, "to", "through", "until", "till":
		joined = joined || connector == andWord
		suffix = strings.TrimLeftFunc(suffix[i:], func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune(`(["'‘’“”«»`, r) })
	}
	r, _ = utf8.DecodeRuneInString(suffix)
	if numericOperator(r) && (arithmeticConnectors[connector] || connector == "per" || connector == "each" || connector == "a" || connector == "an") {
		return true
	}
	if arithmeticConnectors[connector] {
		suffix = strings.ToLower(strings.Join(strings.Fields(suffix), " "))
		if strings.HasPrefix(suffix, "a factor of ") || strings.HasPrefix(suffix, "factor of ") || strings.HasPrefix(suffix, "negative ") || strings.HasPrefix(suffix, "minus ") || strings.HasPrefix(suffix, "≤") || strings.HasPrefix(suffix, "≥") || strings.HasPrefix(suffix, "<=") || strings.HasPrefix(suffix, ">=") || strings.HasPrefix(suffix, "<") || strings.HasPrefix(suffix, ">") || approximationSuffixRE.MatchString(suffix) {
			return true
		}
		for _, q := range boundWords {
			for _, word := range q.words {
				if strings.HasPrefix(strings.ToLower(suffix), word+" ") {
					return true
				}
			}
		}
	}
	if connector == "less" {
		loc := lessOperandRE.FindStringIndex(suffix)
		if loc == nil && f.Kind != durationKind {
			loc = lessBareOperandRE.FindStringIndex(suffix)
		}
		if loc != nil && !quantitySubjectRE.MatchString(suffix[loc[1]:]) {
			return true
		}
		if loc := lessArticleRE.FindStringIndex(suffix); loc != nil {
			for _, re := range []*regexp.Regexp{moneyRE, unitRE, countRE} {
				if figure := re.FindStringIndex(suffix[loc[1]:]); figure != nil && figure[0] == 0 {
					return true
				}
			}
		}
	}
	for _, re := range []*regexp.Regexp{moneyRE, unitRE, countRE} {
		if loc := re.FindStringIndex(suffix); loc != nil && loc[0] == 0 {
			// Only a conjunction or comma can introduce an independent fact.
			return !joined
		}
	}
	if joined {
		if loc := indefiniteQuantityRE.FindStringIndex(suffix); loc != nil {
			tail := suffix[loc[1]:]
			unit := unitRE.FindStringIndex("1 " + tail)
			end := 0
			if unit != nil && unit[0] == 0 {
				end = unit[1] - 2
			} else if unit = currencySubunitRE.FindStringIndex(tail); unit != nil {
				end = unit[1]
			}
			if end > 0 && !quantitySubjectRE.MatchString(tail[end:]) {
				return true
			}
		}
	}
	if connector == andWord || connector == "to" || connector == "through" || connector == "until" || connector == "till" || connector == "per" || connector == "each" || connector == "a" || connector == "an" {
		for _, symbol := range []string{"<=", ">=", "≤", "≥", "<", ">"} {
			if tail, ok := strings.CutPrefix(suffix, symbol); ok {
				return connector != andWord || !independentBoundedFigure(strings.TrimSpace(tail), f)
			}
		}
		if approximationSuffixRE.MatchString(suffix) {
			return connector != andWord || !typedFigureLeadRE.MatchString(strings.TrimSpace(approximationSuffixRE.ReplaceAllString(suffix, "")))
		}
		s := strings.ToLower(strings.Join(strings.Fields(suffix), " "))
		for _, q := range boundWords {
			for _, word := range q.words {
				if s == word || strings.HasPrefix(s, word+" ") {
					if connector == andWord && independentBoundedFigure(strings.TrimSpace(s[len(word):]), f) {
						return false
					}
					return true
				}
			}
		}
	}
	i = strings.IndexFunc(suffix, func(r rune) bool { return !unicode.IsLetter(r) })
	if i < 0 {
		i = len(suffix)
	}
	r, _ = utf8.DecodeRuneInString(suffix)
	return unicode.IsDigit(r) || numeralWord(suffix[:i]) || fractionSuffixRE.MatchString(suffix) || f.Kind == "money" && slices.Contains([]string{"m", "mn", "k"}, strings.ToLower(suffix[:i]))
}

func independentBoundedFigure(s string, prior Figure) bool {
	for _, re := range []*regexp.Regexp{moneyRE, unitRE, countRE} {
		if loc := re.FindStringIndex(s); loc != nil && loc[0] == 0 {
			var f Figure
			var ok bool
			parts := re.FindStringSubmatch(s[:loc[1]])
			switch re {
			case moneyRE:
				f, ok = parseMoney(s[:loc[1]], parts)
			case unitRE:
				f, ok = parseUnit(s[:loc[1]], parts)
			case countRE:
				f, ok = parseCount(s[:loc[1]], parts)
			}
			return ok && (quantitySubjectRE.MatchString(s[loc[1]:]) || f.Kind != prior.Kind || f.Kind == "count" && f.Unit != prior.Unit)
		}
	}
	return false
}

const closingPunctuation = ")]}\"'”’»"

func identifierEndsAt(suffix string) bool {
	return identifierConjunctionRE.MatchString(suffix) || tokenEndsAt(suffix) && !identifierSuffixMaskRE.MatchString(suffix)
}

// A date or identifier must end at prose or detached sentence punctuation,
// not a continued token or further digit-bearing group after whitespace.
func tokenEndsAt(suffix string) bool {
	if suffix == "" {
		return true
	}
	r, n := utf8.DecodeRuneInString(suffix)
	if unicode.IsSpace(r) {
		fields := strings.Fields(suffix)
		return len(fields) == 0 || !strings.ContainsAny(fields[0], "0123456789")
	}
	if strings.ContainsRune(".,;:!?"+closingPunctuation, r) {
		next, _ := utf8.DecodeRuneInString(suffix[n:])
		return len(suffix) == n || unicode.IsSpace(next) || strings.ContainsRune(closingPunctuation, next) || strings.ContainsRune(closingPunctuation, r) && strings.ContainsRune(".,:;!?", next) && tokenEndsAt(suffix[n:])
	}
	return false
}

// A whitespace-separated typed figure may follow a complete date.
func dateEndsAt(suffix string) bool {
	if dateSlashSuffixRE.MatchString(suffix) || incompleteAlternativeDateSuffix(suffix) {
		return false
	}
	if tokenEndsAt(suffix) {
		return true
	}
	if suffix == "" {
		return false
	}
	r, _ := utf8.DecodeRuneInString(suffix)
	if !unicode.IsSpace(r) {
		return false
	}
	suffix = strings.TrimLeftFunc(suffix, unicode.IsSpace)
	for _, re := range []*regexp.Regexp{moneyRE, unitRE, countRE} {
		if loc := re.FindStringIndex(suffix); loc != nil && loc[0] == 0 {
			return true
		}
	}
	return false
}

func dateContinuedPrefix(prefix string) bool {
	prefix = trimDateOpening(prefix)
	r, n := utf8.DecodeLastRuneInString(prefix)
	if !numericOperator(r) {
		return incompleteAlternativeDateRE.MatchString(prefix)
	}
	previous, _ := utf8.DecodeLastRuneInString(strings.TrimRightFunc(prefix[:len(prefix)-n], func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune(closingPunctuation, r) }))
	return unicode.IsDigit(previous)
}

var incompleteAlternativeDateSuffixRE = regexp.MustCompile(`(?i)^[\s,)\]"'‘’“”»]+or\s+[(\["'‘’“”«]*(?:[0-9]{1,2}\s+)?(?:january|february|march|april|may|june|july|august|september|october|november|december)\b`)

var bareAlternativeDayRE = regexp.MustCompile(`(?i)^[\s,)\]"'‘’“”»]+or\s+[(\["'‘’“”«]*[0-9]{1,2}\s*(?:[.?!,;)»]|$)`)

func completeAlternativeDate(s string, end int) bool {
	loc := dateRE.FindStringIndex(s)
	return loc != nil && loc[0] < end && loc[1] > end
}

func incompleteAlternativeDateSuffix(s string) bool {
	loc := incompleteAlternativeDateSuffixRE.FindStringIndex(s)
	if loc == nil {
		return bareAlternativeDayRE.MatchString(s)
	}
	return !completeAlternativeDate(s, loc[1])
}

var incompleteAlternativeDateRE = regexp.MustCompile(`(?i)(?:^|\s)(?:[0-9]{1,2}(?:\s+(?:january|february|march|april|may|june|july|august|september|october|november|december))?|january|february|march|april|may|june|july|august|september|october|november|december)\s+(?:or|to|and)\s*[(\["'‘’“”«]*$`)

var dateSlashSuffixRE = regexp.MustCompile(`(?i)^[\s)\]"'‘’“”»]*[/\p{Pd}⁓⁻₋−]\s*[(\["'‘’“”«]*(?:[0-9]|january\b|february\b|march\b|april\b|may\b|june\b|july\b|august\b|september\b|october\b|november\b|december\b)`)

func numbers(s string, multiplier int64) (string, string, bool) {
	parts := wordRE.FindAllString(strings.ToLower(s), -1)
	if len(parts) < 1 || len(parts) > 2 {
		return "", "", false
	}
	vals := make([]string, 0, len(parts))
	var previous *big.Rat
	for _, p := range parts {
		if len(p) > 128 {
			return "", "", false
		}
		if v, ok := numberWords[p]; ok {
			p = v
		}
		p = strings.ReplaceAll(p, ",", "")
		r, ok := new(big.Rat).SetString(p)
		if !ok {
			return "", "", false
		}
		if previous != nil && previous.Cmp(r) > 0 {
			return "", "", false
		}
		previous = new(big.Rat).Set(r)
		r.Mul(r, big.NewRat(multiplier, 1))
		scale := 0
		if i := strings.IndexByte(p, '.'); i >= 0 {
			scale = len(p) - i - 1
		}
		v := r.FloatString(scale)
		if strings.Contains(v, ".") {
			v = strings.TrimRight(strings.TrimRight(v, "0"), ".")
		}
		vals = append(vals, v)
	}
	if len(vals) == 2 {
		return vals[0], vals[1], true
	}
	return vals[0], "", true
}
func parseMoney(s string, p []string) (Figure, bool) {
	var unit, num string
	if p[1] != "" {
		num = p[1]
		unit = strings.TrimSpace(strings.TrimSuffix(s, p[1]))
	} else {
		num = p[2]
		unit = p[3]
		if num == "" {
			num, unit = p[4], p[5]
		}
	}
	unit = strings.ToLower(unit)
	switch unit {
	case "dollars", "euros", "pounds", "roubles", "rubles":
		unit = strings.TrimSuffix(unit, "s")
	}
	if unit == "ruble" {
		unit = "rouble"
	}
	v, e, ok := numbers(num, 1)
	return Figure{Kind: "money", Unit: unit, Value: v, EndValue: e}, ok
}
func parseUnit(_ string, p []string) (Figure, bool) {
	num, unit := p[1], strings.ToLower(p[2])
	if num == "" {
		num, unit = p[3], p[4]
	}
	unit = strings.TrimPrefix(strings.Join(strings.Fields(unit), " "), "calendar ")
	kind, mul, canonical := durationKind, int64(1), "minutes"
	switch {
	case unit == "%" || unit == percentKind || unit == "per cent":
		kind, canonical = percentKind, percentKind
	case strings.HasPrefix(unit, "percentage"):
		kind, canonical = percentKind, "percentage point"
	case strings.HasPrefix(unit, "business") || strings.HasPrefix(unit, "working"):
		canonical = "business days"
	case strings.HasPrefix(unit, "hour") || strings.HasPrefix(unit, "hr"):
		mul = 60
	case strings.HasPrefix(unit, "day"):
		mul = 1440
	case strings.HasPrefix(unit, "week"):
		mul = 10080
	case strings.HasPrefix(unit, "month"):
		canonical = "months"
	case strings.HasPrefix(unit, "year"):
		canonical = "months"
		mul = 12
	}
	v, e, ok := numbers(num, mul)
	return Figure{Kind: kind, Unit: canonical, Value: v, EndValue: e}, ok
}
func parseCount(_ string, p []string) (Figure, bool) {
	v, e, ok := numbers(p[1], 1)
	if strings.Contains(v, ".") || strings.Contains(e, ".") {
		ok = false
	}
	return Figure{Kind: "count", Unit: singular(strings.ToLower(p[3])), Value: v, EndValue: e, Modifier: strings.ToLower(p[2])}, ok
}
func singular(s string) string {
	if strings.HasSuffix(s, "es") && (strings.HasSuffix(s, "ches") || strings.HasSuffix(s, "sses")) {
		return s[:len(s)-2]
	}
	return strings.TrimSuffix(s, "s")
}
func parseIdentifier(_ string, p []string) (Figure, bool) {
	v := strings.ToLower(p[2])
	if strings.HasSuffix(v, "-") || strings.Contains(v, "*") || identifierMaskRE.MatchString(v) || !strings.ContainsAny(v, "0123456789") {
		return Figure{}, false
	}
	return Figure{Kind: "identifier", Unit: strings.ToLower(strings.Join(strings.Fields(p[1]), " ")), Value: strings.ReplaceAll(v, "-", "")}, true
}
func parseDate(s string, _ []string) (Figure, bool) {
	s = strings.ReplaceAll(s, ",", "")
	fields := strings.Fields(s)
	s = strings.Join(fields, " ")
	precision := "day"
	layouts := []string{"2006-01-02", "2 January 2006", "January 2 2006"}
	if len(fields) == 2 {
		layouts = []string{"January 2006"}
		precision = "month"
	}
	if strings.Contains(s, "/") {
		parts := strings.Split(s, "/")
		day, err := strconv.Atoi(parts[0])
		if err != nil || day <= 12 {
			return Figure{}, false
		}
		layouts = []string{"2/1/2006"}
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			value := t.Format("2006-01-02")
			if precision == "month" {
				value = t.Format("2006-01")
			}
			return Figure{Kind: dateKind, Unit: "date", Value: value, Precision: precision}, true
		}
	}
	return Figure{}, false
}
