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
without another figure remains a date value with day or month precision. A file's modification date is a
separate source property and is not inferred by this package.
If a clause states different dates for the same period bound, extraction omits
the clause rather than assigning the final date to every figure.
The search report date facility extracts dates from verified, identity-bound artifact
text under its own budget and evidence rules; it serves report construction rather
than this passage-level figure and period extraction. Both use Go date parsing.
Date parsing accepts spaces and tabs while preserving the original quote.
Continued date tokens such as `2026-07-01-02` and `1 July 2026/2027` supply
neither a date value nor a period bound, including two dates joined by a slash.
A complete date can precede an independent typed figure in the same clause.
An accepted date after `to` supplies an end bound, even when its day follows a
money amount, unless its scope is uncertain.

Each figure retains the extraction-accepted date spans as byte offsets in its
normalized clause for same-package consumers. Consumers use these decisions
instead of recognizing dates again, so rejected dates do not silently become
accepted period evidence or text removed from a subject.

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
Both Unicode ellipses and ASCII runs of three or more dots mark adjacent
fragments as partial. Negation detection recognizes `not`, `cannot`, `never`,
`neither`, `nor`, `no`, `without`, `no longer`, `anything but`, and contractions
with ASCII or typographic apostrophes. `no more than` and
`no less than` remain bounds, and `No.` remains an identifier label.
`less than or equal to` and `greater than or equal to` are inclusive bounds,
as are `maximum of` and `minimum of`. `Exceed`, `exceeds`, `exceeded`,
and `exceeding` mark an exclusive lower bound when they precede a figure.
Sentence punctuation followed by closing quotes or brackets ends a clause. Soft line wraps stay in the same clause; blank lines and list or table rows separate clauses.
Approximation markers (`about`, `approximately`, `around`, `roughly`, `nearly`,
`almost`, `circa`, `estimated`, `~`, and `≈`) before or after a figure cause its
omission, including markers before a bound or inside a range. The suffixes
`more or less` and `and change` also mark a figure as approximate. A marker followed
by `exactly`, as in `almost exactly`, or `a total of` also omits the figure. An approximate component omits a
compound duration joined by `and` or a comma, while an independently stated exact figure can remain.
An unsupported numeric alternative such as `7 or 10 days` never becomes the
exact value `10 days`. Unsupported sign words such as `minus` and `negative`
before a numeric figure or its bound never yield a positive value. Postfix
bounds such as `7 days maximum` are omitted rather than treated as exact.
Recognition never rewrites the quoted source text.

Money, percentages, durations and counts share one continuation check. When a
recognized match is part of an unsupported numeric expression, extraction omits
the entire clause, including later figures. Rejected numeric ranges and
overlong numeric tokens follow the same rule. Examples include arithmetic such as
`$7 + $10`, `$100 less $10`, `5 days less than 10 days`, or `50% off $100`, uncertainty such as `7 ± 1 days`, repeated-unit ranges such as `7 days - 10 days` or
`7 days through 10 days`, malformed unit tokens such as `7%2`, and fractions
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
Compound clock durations such as `7 days and 3 hours` retain both components.
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
Arithmetic following a rate still causes the whole clause to be omitted.
Unsupported fractions and numerals grouped with spaces, comma-space, underscores or apostrophes
never produce a smaller component value. A digit-bearing or all-uppercase identifier group after whitespace causes
omission rather than retaining only the first group.

Passages over 4,096 bytes are skipped in full. A clause with more than 64
recognizer matches is skipped, even if some matches overlap or would be rejected;
other clauses remain eligible. Numeric tokens over 128 characters are rejected.
These limits bound work and avoid extracting a misleading prefix.
Normalized values may exceed that source-token limit after unit conversion.
The tests cover original offsets and normalization with Go's native fuzzing.
