# Markdown note structure

`notemarkdown.Parse(ctx, src, Limits{})` reads one note's original UTF-8 bytes.
It returns frontmatter, a title, headings, link occurrences, line counts and
bounded diagnostics. It performs no I/O, rendering, storage or target resolution.
The recipe is `docbank-note-markdown/v2`. Consumers choose which files are notes
and supply a filename title when `TitleSource` is empty.

Every offset addresses the original bytes. A BOM is skipped for parsing only.
CRLF and bare CR keep their original widths. Spans are half-open. Lines are
one-based, ordinals are zero-based, and a trailing line ending creates an empty
final line. `BodyStart` follows the closing frontmatter fence. A heading's
`Start` includes its physical line's container markers; `LineEnd` includes the
complete heading and its line ending (also the underline for Setext).
`SectionEnd` is the next equal-or-higher-level heading's start, or EOF.
`SectionEndLine` is the line at that exclusive boundary. `Path` joins actual
ancestor heading texts with ` > `, skipping absent levels.

Goldmark supplies ATX, Setext, paragraph, list, blockquote, code and HTML block
boundaries. Headings can occur at any container depth. Fenced and indented code,
HTML blocks and frontmatter do not contribute headings or links. Inline code
and HTML syntax hide their contents from the link scanner.

Frontmatter opens only with `---` on the first line, after an optional BOM.
It closes with `---` or `...`. `Start` and `End` include the fences. Valid YAML
produces a JSON-compatible `Raw` map with RFC 3339 timestamp strings. Bounded
aliases and merge maps/sequences are supported: explicit keys win; earlier
merge-sequence maps win. Duplicate keys, cyclic aliases, custom tags, non-string
map keys and non-finite numbers produce `invalid_frontmatter`, with `Raw=nil`
and no fields. The body still parses. An unclosed fence produces
`unclosed_frontmatter`; only the opener is excluded from body parsing.

`Fields` sorts top-level keys by byte order. Each scalar or scalar list item
contributes a row; its ordinal is the original list index (zero for a scalar).
Maps, nested lists and nulls remain in `Raw` and contribute no row. Strings stay
as authored after YAML decoding; booleans use lowercase, numbers use decimal,
and dates use RFC 3339. `Aliases` and `Tags` use those same scalar rows. A
nonempty frontmatter title wins, then the first H1's display text, then the
caller supplies the filename stem. Heading slugs normalize to NFC, lowercase,
retain letters/digits/hyphens, replace whitespace with hyphens and drop other
punctuation. Empty slugs use `section`; global collisions gain numeric suffixes.

Links include inline, reference, image, autolink, wikilink and embed syntax.
Forms are `markdown`, `reference`, `image`, `wikilink` and `embed`. `Raw` is the
whole authored occurrence; `Label` is its display text. Syntax escapes/entities
are decoded where CommonMark allows them, then percent escapes are decoded
once. `Target` and `Fragment` split at the first decoded `#`. Invalid percent
escapes, escapes producing invalid UTF-8 and encoded NUL retain their literal
spelling. Literal targets such as `A:notes.md`, external URLs and decoded path
separators are returned without path or namespace classification.

Zero limits select these hard defaults; positive values can narrow them.
Negative values and values above the defaults return `ErrInvalidInput`.

| Field | Default |
| --- | ---: |
| `MaxBytes` | 4 MiB |
| `MaxHeadings` | 4,096 |
| `MaxLinks` | 10,000 |
| `MaxFrontmatterKeys` | 64 (including merged keys) |
| `MaxValuesPerKey` | 256 |
| `MaxValueBytes` | 1 KiB |
| `MaxDiagnostics` | 32 |

Value and sequence limits also protect nested YAML retained in `Raw`. Internal
limits bound frontmatter to 64 KiB, YAML to depth 32 and 8,192 expanded nodes,
source line breaks to 65,536, AST depth to 128 and metered nodes to 262,144,
link targets to 4,096 bytes, aggregate derived strings to 16 MiB and metered work
to 64 Mi steps. These bounds can reject a note before its public limits are
reached. Work checkpoints observe caller cancellation. Goldmark callbacks
bound block work; YAML syntax decoding sees at most 64 KiB before returning to
the cancellable node traversal. Diagnostics carry fixed codes and raw spans,
never parser error text. A parse currently emits at most one diagnostic.

Any error returns a zero `Note`: invalid UTF-8, source NUL and invalid limits
wrap `ErrInvalidInput`; resource failures are `*LimitError` wrapping `ErrLimit`;
cancellation preserves the context error. Malformed content uses diagnostics.

The package is an unwired structure parser for note projections. Its synthetic
table and property tests cover source offsets, container boundaries, limits,
JSON-safe frontmatter and deterministic cancellation. Run them in the project's
hosted verification lanes with `go test -tags fts5 ./document/notemarkdown/...`.
