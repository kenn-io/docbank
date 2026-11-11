// Package notemarkdown projects original UTF-8 Markdown into frontmatter,
// headings and link occurrences without I/O, rendering or target resolution.
// Recipe identifies these semantics; callers decide which files are notes.
//
// Offsets are half-open original byte positions, including BOM and CRLF/bare CR.
// Lines are one-based; ordinals are zero-based. A trailing EOL adds an empty
// line. Heading Start includes physical container markers; LineEnd includes the
// EOL and Setext underline. SectionEnd is the next equal/higher heading or EOF;
// SectionEndLine names that exclusive boundary. Path joins ancestors with " > ".
// Goldmark supplies block structure at any depth. Code, HTML blocks and
// frontmatter suppress occurrences; inline code and HTML syntax hide links.
// Text and Label collapse whitespace in raw authored inline source, retaining
// emphasis, escapes, entities and link syntax. Slugs NFC-normalize that Text,
// lowercase letters/digits/hyphens, turn spaces into hyphens and drop other
// characters. Empty slugs use "section"; global collisions gain numeric suffixes.
// These are parser keys with no GitHub or Obsidian anchor compatibility promise.
//
// Frontmatter opens with --- on line one after an optional BOM and closes with
// --- or .... Its span includes fences; BodyStart follows the closing line.
// Invalid/unsupported YAML yields invalid_frontmatter, nil Raw and no fields;
// the body starts after the fence. An unclosed opener yields unclosed_frontmatter
// and excludes only the opener. Diagnostics contain fixed codes and raw spans;
// admission enforces MaxDiagnostics, though currently at most one is emitted.
// Raw is JSON-compatible: timestamps use RFC 3339; aliases and merges are bounded,
// explicit keys win, and earlier merge maps win. Duplicate/non-string keys,
// cycles, custom tags and non-finite numbers are invalid. Fields sort top-level
// keys and flatten scalars/lists with original list ordinals; maps, nested lists
// and nulls stay only in Raw. Bools use lowercase, numbers decimal, dates RFC 3339.
// Aliases/Tags use the corresponding scalar rows. A nonempty frontmatter title
// wins, else the first H1 even when empty; callers use the filename for empty Title.
//
// Forms are markdown (including autolinks), reference, image, wikilink and embed.
// Raw spans the whole occurrence. Targets decode Markdown syntax where allowed,
// split at the first structural #, then percent-decode each component once.
// Autolinks retain literal entity/backslash spelling. Invalid escapes, decoded
// invalid UTF-8 or NUL retain that component's literal spelling. Encoded hashes
// stay in filenames: A%23B.md#H%23x becomes Target A#B.md, Fragment H#x.
// Literal A:notes.md, external URLs and path separators are not classified here.
//
// Zero limits select hard defaults: 4 MiB input, 4096 headings, 10000 links,
// 64 frontmatter keys (including merged keys), 256 values/key, 1024 bytes/value,
// 32 diagnostics. Positive limits narrow; negative/above-hard limits are invalid.
// Value/sequence limits apply to nested Raw values too, but not mapping keys.
// Internal bounds also apply: YAML depth 32/8192 expanded nodes, AST depth
// 128/262144 metered nodes, 4096-byte targets, 16 MiB derived strings and 64 Mi
// work steps. Syntax input and the line index are bounded by MaxBytes, without
// separate frontmatter-byte/line-count caps. YAML syntax reads metered chunks;
// alias expansion and Markdown scans check cancellation at work checkpoints.
// Any error returns a zero Note: invalid UTF-8/NUL/limits use ErrInvalidInput,
// resource failures use LimitError wrapping ErrLimit, cancellation preserves
// the context error. Oversize YAML values are errors, never diagnostics.
package notemarkdown
