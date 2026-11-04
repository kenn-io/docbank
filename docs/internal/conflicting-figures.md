# Typed figures in passage text

`internal/conflicts.Extract` extracts figures from short English passages for
in-process consumers. It performs no I/O and imports no daemon or storage code.
It recognizes money, percentages, durations, a bounded set of counts, dates and
labelled identifiers. It does not decide which statement is correct.

## Preserve the source text

Each figure carries its original text and clause, with rune offsets into the
input. NFKC normalization is used for recognition, while a boundary map keeps
those offsets anchored to the original input. Matches that split a normalization
segment are discarded. Values use exact decimal arithmetic; clock durations
normalize to minutes, calendar durations to months, and business days remain
separate.

Dates in a clause with another figure become stated period bounds when the
clause supplies a recognized period word, including `effective on` and `starting on`. Period bounds are inclusive: `before`
and `prior to` use the preceding calendar day, including the last day of the
previous month for a month-precision date. `on or before` includes the stated
day or month. A duration immediately before a
`before` or `prior to` date can state a relative deadline, so its period is
unknown. When an exclusive bound cannot be represented, or the bounds are inverted, the whole period
is unknown. Month-precision end bounds include their entire month. Independent
assertions with unclear date scope also receive an unknown period. A date
without another figure remains a date value with day or month precision,
unless it is a standalone bound such as `before 1 July 2026` or
`before 1 July 2026 or 2 August 2026`; those dates are omitted rather than
compared as exact values. Standalone intervals such as `1 July 2026 through
31 July 2026`, `1 July 2026 until 31 July 2026`, and
`1 July 2026 till 31 July 2026` also omit both endpoints, including
`From 1 July 2026 to 31 July 2026` when no other figure appears in the clause.
Explicit period bounds remain usable, as in
`From 1 July 2026 through 31 July 2026, the fee is $5`, which retains `$5`
with both bounds. A file's modification date is a separate source property
and is not inferred by this package.
If a clause states different dates for the same period bound, extraction omits
the clause rather than assigning the final date to every figure.
The search report date facility extracts dates from verified, identity-bound artifact
text under its own budget and evidence rules; it serves report construction rather
than this passage-level figure and period extraction. Both use Go date parsing.
Date parsing accepts spaces and tabs while preserving the original quote.
Continued date tokens such as `2026-07-01-02` and `1 July 2026/2027` supply
neither a date value nor a period bound, including two dates joined by a slash.
A short code prefix in a labelled identifier also prevents an embedded date
from becoming a date value, as in `Policy: AB/2026-07-01` or
`Policy: A/B/2026-07-01`. A complete date can
precede an independent typed figure in the same clause.
An accepted date after `to` supplies an end bound, even when its day follows a
money amount, unless its scope is uncertain.
Period words after an abbreviated rate are matched without regard to case, so
`The fee is $5/hr. BEFORE 1 July 2026.` retains the exclusive end bound.
Supported date wrappers remain part of that bound, so
`The fee is $5/hr. before (1 July 2026).` also retains it.
Closing quotes after a terminal mark stay with that bound, so
`«The fee is $5/hr. before 1 July 2026.»` also retains it. A quote after the
abbreviated rate may precede the bound connector, as in
`The fee is "$5/hr." until 1 July 2026.`
The bound also survives a closing quote before an independent sentence, as in
`«The fee is $5/hr. before 1 July 2026.» The deposit is $7.`
The bound also survives when the clause ends at that date or a semicolon, as in
`The fee is $5/hr. before 1 July 2026` and
`The fee is $5/hr. before 1 July 2026;`. Parenthesized endpoints in a stated
two-date interval remain an interval: `The wait is 7 days from (1 July 2026)
until (31 July 2026).` and `The wait is 7 days from (1 July 2026) and ending
(31 July 2026).` retain both bounds.

Each figure retains the extraction-accepted date spans as byte offsets in its
normalized clause for same-package consumers. Consumers use these decisions
instead of recognizing dates again, so rejected dates do not silently become
accepted period evidence or text removed from a subject.
Subject preparation removes only accepted date spans; rejected date-shaped text
and years remain as subject discriminators.

## Prefer omission to an ambiguous value

Extraction skips clauses containing a pipe, negated or example clauses, approximate figures and
ambiguous overlapping recognizers. Clauses with letters but no Latin letters are skipped;
letter-free typed figures such as `$7`, `7%`, and `2026-07-01` are recognized.
Bare numbers are not figures. Count nouns
and modifiers use the explicit vocabulary in the package; adding a word changes
the recognition contract and needs positive and negative regression coverage.
Currency symbols and ISO codes remain distinct, so a dollar symbol does not
imply USD. A clause with a combined code and symbol, such as `CAD $5`, is
omitted rather than assigning it a currency. Partial snippet clauses are marked for callers to exclude.
Unicode ellipses and runs of three or more ASCII or normalization-equivalent
fullwidth periods mark adjacent fragments as partial. Negation detection
recognizes `not`, `cannot`, `never`,
`neither`, `nor`, `no`, `without`, `no longer`, `anything but`, and contractions
with ASCII or typographic apostrophes. `no more than` and
`no less than` remain bounds, and `No.` remains an identifier label.
`less than or equal to` and `greater than or equal to` are inclusive bounds,
as are `maximum of` and `minimum of`. `Exceed`, `exceeds`, `exceeded`,
and `exceeding` mark an exclusive lower bound when they precede a figure.
Comparison continuations omit the clause even when the right operand cannot be
parsed. This includes symbolic comparisons after a rate; word-number or
fraction-word operands such as `$5/month higher than thirty`, `$5/month greater
than thirty dollars`, `$5/month higher than half a dollar`, or
`$5/month higher than an estimated $10/month`; spelled-out
duration operands such as `7 days before thirty days` or
`7 min. before thirty-five min.`; grouped numeric, fractional or spoken-decimal
durations such as `7 min. before 1 000 minutes`,
`7 min. before 1 000.5 minutes`, `7 min. before 1/2 hour`,
`7 min. before ½ hour`,
`7 min. before 1'000 minutes`, `7 min. before 1_000 minutes`,
`7 min. before 1ʼ000 minutes`,
`7 min. before 1, 000 minutes`, or `7 min. before 1  000 minutes`,
`7 min. before .5 hours`, `7 min. before half an hour`,
`7 min. before seven point five minutes`, or signed operands such as
`7 min. before +10 min.`, `7 min. before negative 10 min.`, and
`7 min. before −10 min.`; rejected ranges
such as `7 min. before 10-7 min.`; and wrapped bounds such as
`7 min. before (about 10 min.)` or
`(7 days) before (about 10 days)`. Spaces before a closing wrapper do not hide
the connector, so `(7 days ) before (about 10 days)` remains one unsupported
comparison.
Modified and possessive baselines such as `last year's rate`, temporal
baselines such as `last year`, and pronoun baselines such as `it was` also keep
the comparison amount unresolved.
Following sentences remain independent.
Auxiliary-led text such as `10 minutes have elapsed` continues the duration
operand. The full expression `The deadline is 7 min. before 10 minutes have
elapsed.` yields no figures. A malformed numeric operand such as `1,00 minutes`
or `1 00 minutes` is also unsupported: `The deadline is 7 min. before 1,00
minutes.` and `The deadline is 7 min. before 1 00 minutes.` yield no figures.
A malformed mixed-separator operand such as `1, 00 minutes` is also
unsupported: `The deadline is 7 min. before 1, 00 minutes.` yields no figures.
Arithmetic-shaped relative operands stay attached across an abbreviated unit,
including mixed operator chains and number words: `The deadline is 7 min.
before 10 + 5 - 2 min. The deposit is $7.` and `The deadline is 7 min. before
ten + five − two min. The deposit is $7.` omit the relative comparison and
keep the independent `$7`.
Complements such as `including breaks` and `excluding breaks` stay attached to
the duration operand, so `The deadline is 7 min. before 10 minutes including
breaks.` and `The deadline is 7 min. before 10 minutes excluding breaks.`
yield no figures.

An independent temporal assertion after an abbreviated duration remains
separate when it starts with its own subject and predicate, or with a typed
figure followed directly by an assertion predicate. For example, `The wait is
7 min. After 10 minutes, $5 is charged.`, `The wait is 7 min. After 1/2 hour
customers pay $5.`, `The wait is 7 min. After ½ hour customers pay $5.`, and
`The wait is 7 min. After 1 000 minutes customers pay
$5.`, `The wait is 7 min. After seven point five minutes,
$5 is charged.`, and `The wait is 7 min. After twenty-one minutes customers
pay $5.` keep both figures, even when the duration operand itself is rejected.
`The wait is 7 min. After 10 minutes customers pay cash.` keeps the preceding
`7 min` in its own clause even though the later predicate has no following amount.
A main-clause assertion after a comma also stays separate from a temporal
subordinate clause: `The wait is 7 min. Before 10 minutes elapse, customers pay
cash.` keeps `7 min` in its own clause.
An `and <duration> later` continuation also stays separate when the later
assertion starts with its own subject and predicate or a typed figure. For
example, `The wait is 7 min. and 1 000 minutes later the fee is $5.` and `The
wait is 7 min. and an hour later, $5 is charged.` keep `7 min` and `$5`.
A signed duration in an independent later assertion also stays separate, as in
`The wait is 7 min. and +10 minutes later the fee is $5.` Figures asserted
after an approximate amount or percentage base remain separate too.
`The fee is about $5 and $10 covers insurance.` and `The fee is about $5 and
$10 pays shipping costs.` and `The fee is about $5 and $10 includes shipping
costs.` keep `$10`; `The fee is 5% of $100 and $200 covers insurance.` and `The
fee is 5% of $100 and $200 pays shipping costs.` keep `5%` and `$200`. `The
fee is 5% of $100 and $10 includes shipping costs.` keeps `5%` and `$10`.
Independent predicates also remain separate: `The fee was about $5 and $10
covered shipping costs.` keeps `$10`, and `The fee is 5% of $100 and $10
offsets shipping costs.` keeps `5%` and `$10`. A malformed operand
does not discard a later independent assertion: `The wait is 7 min. After 1,00
minutes customers pay $5.` and `The wait is 7 min. After 1 00 minutes customers
pay $5.` keep `7 min` and `$5`.
The mixed separator form also preserves that later assertion: `The wait is 7
min. After 1, 00 minutes customers pay $5.` keeps both figures.
Assertion lookahead ends at sentence punctuation. Thus `The deadline is 7 min.
before 10 minutes. The fee is $5.` omits the relative comparison and keeps only
`$5`; `The wait is 7 min. approximately. $5 is charged.` also keeps only `$5`.
Closing wrappers and their surrounding whitespace do not split arithmetic
continuations: `The wait is (7 min. ) + 3 min.` yields no figures. The same
wrapper before an independent sentence preserves both assertions, as in `The
wait is (7 min. ) The fee is $5.`
Nominal phrases such as `shipping charges`, `access fees`, `express delivery
charges`, or `shipping and handling charges` stay inside an approximate group:
`The fees are about $5 and $10 shipping charges.` and `The fees are about 5%
of $100 and $200 shipping charges.` yield no figures. The coordinated nominal
form `The fees are about $5 and $10 shipping and handling charges.` also yields
no figures. Modified nominal continuations such as `fixed monthly charges`,
`sales taxes`, and `customs duties` stay inside the same approximate group; for
example, `The fees are about $5 and $10 fixed monthly charges.` and `The fees
are about $5 and $10 customs duties.` yield no figures. In a percentage-base
phrase, the rate remains while the nominal continuation operands are omitted:
`The fee is 5% of $100 and $200 fixed charges.`, `The fee is 5% of $100 and
$200 fixed monthly charges.`, `The fee is 5% of $100 and $200 sales taxes.`,
`The fee is 5% of $100 and $200 customs duties.`, `The fee is 5% of $100 and
$200 shipping and handling charges.`, `The fee is 5% of $100 and $200 access
fees.`, and `The fee is 5% of $100 and $200 express delivery charges.` keep
`5%` only.
Ambiguous predicate/noun forms such as `charges` and `costs` do not end those
groups: `The plans require about $5 and $10 charges.` yields no figures, while
`The surcharge is 5% of the $100 and $200 costs.` keeps `5%` only. Clear
predicates such as `covers shipping costs` still identify an independent
assertion. `Cost` followed by a typed amount also identifies a predicate:
`The plan includes about 30 devices and 2 devices cost $10.` keeps `2 devices`
and `$10`, while a bare nominal `costs` remains part of the preceding group.
The same holds for grouped operands such as `After 1'000 minutes`, a
parenthesized operand such as `and (an hour) later`, and a quoted typed figure
such as `After 10 minutes, "$5" is charged.` Independent bounded assertions
such as `More than 2 devices are included` and `Less than 2 devices are
included` also start a new sentence after an abbreviated duration. Unsupported
signed temporal operands such as
`After negative 10 minutes` or `After −10 minutes` are excluded while a
separate fee assertion can remain.
Closing wrappers after the duration do not hide the following assertion:
`The wait is 7 min. After (10 minutes), $5 is charged.` also keeps both figures.
A coordinated continuation does not start an independent assertion, even when
it names a new subject: `The deadline is 7 min. before 10 minutes and the
monthly fee is $5.` remains one ambiguous clause and yields no figures.
Sentence punctuation followed by closing quotes or brackets ends a clause,
even when whitespace separates the punctuation and wrapper. The wrapper remains
in the clause, so `The adjustment is ($5/hr. )` yields no figure, while a
separate following sentence remains eligible. Soft line wraps stay in the same
clause; blank lines and list or table rows separate clauses.
Approximation markers (`about`, `approximately`, `around`, `roughly`, `nearly`,
`almost`, `circa`, `estimated`, `~`, and `≈`) before or after a figure cause its
omission, including markers before a bound or inside a range. The suffixes
`more or less` and `and change` also mark a figure as approximate. A marker followed
by `exactly`, as in `almost exactly`, or `a total of` also omits the figure. An approximate component omits a
compound duration joined by `and` or a comma, while an independently stated exact figure can remain.
Unsupported spoken operands remain attached too: `There are about thirty
devices and 2 users.`, `The rate is about thirty percent and 5 percent.`,
`The wait is almost exactly thirty minutes and 2 hours.`, `The wait is at least
thirty minutes and 2 hours.`, and `The wait is ≈thirty minutes and 2 hours.`
yield no figures. An unsupported spoken count, duration, or amount in a
percentage base also keeps following operands in that base, including after a
bound: `The fee is 5% of thirty devices and 2 devices.`, `The fee is 5% of at
least thirty dollars and $200.`, and `The fee is 5% of at least thirty minutes
and 2 hours.` keep `5%` only. A later sentence remains independent.
An unsupported numeric or verbal duration alternative such as `7 or 10 days`,
`7 min. or an hour`, `7 min. or .5 hours`, or `7 min. and thirty minutes`
never becomes a partial exact value. Unsupported sign words such as `minus`
and `negative`
before a numeric figure or its bound never yield a positive value. Postfix
bounds such as `7 days maximum` are omitted rather than treated as exact.
Postfix modifiers remain active inside guillemets: `7 days «approximately»`,
`7 days «maximum»`, `7 days «excluding weekends»`, and `7 days «give or take»`
all omit the duration.
Signed alternatives such as `7 min. or +10 min.` and `7 min. or negative 10
min.` also yield no figures.
Recognition never rewrites the quoted source text.

Money, percentages, durations and counts share one continuation check. When a
recognized match is part of an unsupported numeric expression, extraction omits
the entire clause, including later figures. Rejected numeric ranges and
overlong numeric tokens follow the same rule. Examples include arithmetic such as
`$7 + $10`, `$100 less $10`, `5 days less than 10 days`, or `50% off $100`,
uncertainty such as `7 ± 1 days`, repeated-unit ranges such as `7 days - 10 days`
or `7 days through 10 days`, malformed unit tokens such as `7%2`, and fractions
such as `7 days and a half`. An interrupted
range such as `$7 to about $10` or `between 7 and at most 10 days` never leaves
an exact or bounded endpoint.

Compound money such as `$7 and fifty cents`, `7 dollars, fifty cents`, or
`$7 plus 50 cents` follows the same omission rule. The extractor does not parse
cents or convert currencies. Complete supported ranges such as `7-10 days`
remain one range figure. A bound on a range, such as `at least 5-10 days`, is
ambiguous and is omitted. Number-word endpoints also support an en dash or
ASCII hyphen, as in `seven–ten days` and `one-two days`. Reversed ranges and
unsupported compound numerals such as `twenty-one` are omitted.
Other Unicode dash characters do not form supported ranges and do not yield
exact endpoints.
An independently typed figure after `and`, such as `$7 and 2 devices`, remains
separate. Identifiers and dates take precedence over embedded money fragments.
A standalone labelled numeric value such as `Account: 1234` is an identifier.
When a bare label precedes a recognized numeric amount or quantity, extraction
returns the numeric figure. An explicit `number`, `no.` or `ID` marker retains
identifier precedence.
Rejected identifiers retain that precedence: a mask or unsupported continuation
does not turn embedded currency or date text into another figure.
Compound clock durations such as `7 days and 3 hours` retain both components;
comparison skips clauses with multiple independent figures rather than
assigning components to subjects.
An approximation attached to coordinated figures of the same kind omits the
affected group.
A duration followed by `excluding`, `except`, or `less` is omitted, including joined
components, regardless of the exclusion's object. Independent assertions remain
separate; the extractor does not infer a business calendar from exclusions. Explicit
`business days` and `working days` remain a separate unit, and `calendar days`
normalize as ordinary days.
A slash rate such as `$5/month` or `$5/2 devices` retains the numerator and
any recognized numeric denominator for later rate classification. A denominator
must include a noun; a bare numeric divisor remains an unsupported fraction.
Unsupported fractions or arithmetic following a rate still cause the whole
clause to be omitted. A postfix bound on a rate also suppresses its denominator
across wrapped whitespace, as in `$5 [maximum  ] per 2 devices`.
Unsupported fractions and numerals grouped with spaces, comma-space, underscores
or apostrophes never produce a smaller component value. A digit-bearing or
all-uppercase identifier group after whitespace causes
omission rather than retaining only the first group.

Passages over 4,096 bytes are skipped in full. A clause with more than 64
recognizer matches is skipped, even if some matches overlap or would be rejected;
other clauses remain eligible. Numeric tokens over 128 characters are rejected.
These limits bound work and avoid extracting a misleading prefix.
Normalized values may exceed that source-token limit after unit conversion.
The tests cover original offsets and normalization with Go's native fuzzing.

## Compare figures across documents

`Find` compares eligible figures from different caller-supplied document
identities. The kinds, units, qualifiers, and range shapes must match. Shared
query terms, subject overlap, and preserved discriminators restrict alignment.
Named variants, years, free versus paid entitlements, and different units remain
separate.

Clauses with multiple independent figures are skipped instead of assigning
figures to subjects by position. Numeric rate denominators do not count as
independent figures. When partial or ambiguous figures leave no eligible figure,
`Skipped` reports `partial` or `ambiguous`. An eligible clause keeps its passage
available for comparison.

Rates require matching denominators. The bounded grammar recognizes `per`,
`each`, `/`, `a`, and `an` immediately after a figure, followed by an optional
number or range, an optional time/count modifier (`business`, `working`,
`calendar`, `free`, `paid`, `additional`, `extra`), and one noun. The denominator
is retained separately from the subject's word set, so shared nouns elsewhere
cannot hide its role. Accepted rate spans are removed from the subject, so
`$5 per month`, `$5/month`, and `$5 a month` agree. Numeric denominator figures
are excluded from independent comparison. Thus `$5 per month` and `$60 per year`
are not comparable; rate conversion is outside this package's scope. Ordinary
duration conversions such as `24 hours` and `1 day` still agree, and differing
numerators with the same denominator can conflict. Closing quotes around a
numerator stay attached to its rate, so `"$5" a month` and `"$6" a year` are
not compared as flat amounts. Accepted period connectors after a denominator,
including `prior to`, remain valid; runs of spaces or tabs in multiword
connectors such as `on or before` normalize before comparison. Unsupported
fractions or arithmetic after a rate omit the whole clause. Additional
denominator words, such as `per passenger mile`, are unsupported. Denominator
extensions such as
`per unit of energy`, `per bottle of water`, and `per bottle (of water)` are
omitted instead of compared using only the first noun. Incomplete composite
abbreviations such as `sq.` and `cu.` are also omitted instead of treating the
prefix as a complete denominator. A second article rate after a supported
denominator is also unsupported. Wrapped article
denominators such as `a (month)`,
`a [month]`, or `a "month"` are omitted rather than compared as unqualified
amounts.

Clauses with multiple rates or unbound `per`, `each`, `every`, or `/` markers are
skipped. This includes prefix rates such as `Per device the fee is $5.`

Each reported conflict is an explicit pair with exact source spans. Date
precision and subject similarity are not transitive, so the package does not
merge chains of pairwise matches into one claim. Month-precision dates can agree
with a date in that month without making different days equivalent. Stated
periods may explain a dated change but never select a winner.

The report distinguishes `CONFLICTING`, `NO_CONFLICT`, and `NOT_COMPARABLE`.
`NO_CONFLICT` covers only aligned figures in a complete comparison. `Find`
examines at most 20 passages and 10,000 candidate pairs, and retains at most 100
conflict edges. `Truncated` marks incomplete input, work, or output; an incomplete
comparison without a retained conflict is `NOT_COMPARABLE`. Cutoffs follow input
rank, so truncated subsets may change when rank changes.
