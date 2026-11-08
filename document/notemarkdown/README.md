# Authored Markdown notes

`notemarkdown` derives note metadata, headings and explicit links from retained
original bytes. It also plans logical link targets and resolves bounded candidate
evidence. It does no I/O and has no vault, path, environment, clock or provider
access. Applications supply authorized namespace and exact-version identities.

## Parse an original

Call `Parse(ctx, original)` with the exact original UTF-8 bytes. The result
records their SHA-256 and byte count. Parsing preserves BOM, CRLF and Unicode
spelling. `Span` uses half-open **original UTF-8 byte offsets**. A reference link's
use span and destination-definition span can be in different parts of the file.
Heading sections end at the next same-or-shallower root heading or the original's
end. These offsets are independent of rendition citation code points.

Authored frontmatter is an optional opening YAML mapping after an optional BOM.
Standalone `---` opens it; `---` or `...` closes it. The bounded YAML subset
supports plain, single/double-quoted and literal/folded strings, flow/block
sequences and mappings, including an empty flow mapping. Multiline plain scalars
and explicit block-scalar indentation indicators are unsupported and invalidate
metadata. Only a string `title`, string/string-list `aliases`, and
string/string-list `tags` enter trusted metadata. Unknown fields stay in original
bytes. Duplicate keys, nonstring metadata, aliases, merge keys, custom tags and
malformed syntax invalidate the entire metadata group. A delimited invalid header
is excluded from the body; an unclosed header is diagnosed and scanned as ordinary
Markdown. A `docbank` key grants no provenance or authorization.

The package returns the authored title separately. The adapter's display fallback
is nonempty authored title, first root H1 display text, then publisher basename.
It does not insert a basename or arbitrary frontmatter fields into the artifact.

Root CommonMark ATX/Setext headings expose source Markdown and display text.
A new ordered list interrupts a paragraph only with start number 1. Other
numbers can start lists at block boundaries or continue an existing list.
Code, YAML, raw HTML and headings inside list/quote containers do not create root
sections. Heading display handles emphasis, exact code runs, admitted link/image
labels, wiki labels, entities and inline HTML. It collapses display whitespace.
Anchors normalize display text to NFC, lowercase Unicode letters/digits, turn
whitespace into hyphens and remove punctuation except literal hyphens. An empty
base becomes `section`. Source-order allocation is globally collision-free:
`A`, `A`, `A-1` become `a`, `a-1`, `a-1-1`. This is a versioned Docbank recipe,
not a promise of another application's anchor parity.

Links include CommonMark inline/reference links and images, URI/email autolinks,
`[[target]]`, `[[target#heading|label]]`, `[[#heading]]` and `![[target]]`.
Keep every authored occurrence. Reference definitions alone, code, escaped
openers and HTML syntax do not create edges. An embed only records authorship;
it never fetches or transcludes bytes. Links and reference definitions may
continue across lines in the same list or quote paragraph. Their spans retain
the original container prefixes; prefixes do not become destination bytes.
CommonMark destinations decode punctuation
escapes and entities once. Autolink interiors are literal. Wiki targets keep
entities literal and unescape only `\\`, `\]` and `\|`. URI percent decoding
belongs to target planning, not parsing. Bare URLs and semantic similarity do not
create authored links.

## Plan and resolve a target

`PlanTarget(sourcePath, link)` takes a canonical relative slash source path and
a syntax-decoded target. It splits literal query/fragment syntax before one
percent-decoding pass. `%252F` stays literal `%2F`; encoded slash/backslash/NUL,
invalid UTF-8, malformed escapes, drive/UNC spellings and namespace escapes are
invalid. A literal interior backslash stays a filename character. Paths preserve
case and Unicode bytes on every OS; no filesystem normalization occurs.

Relative Markdown paths start at the source directory. Empty or fragment-only
targets select the source itself. Wiki `/target` starts at the publication root.
Only extensionless wiki targets gain `.md` completion. Bare extensionless wiki
names also plan exact basename and alias fallback keys. Explicit extensions and
paths gain no fallback; no `.markdown` extension is guessed. Explicit non-Markdown
extensions are attachments. URI schemes/protocol-relative targets are external;
query strings and `^block` fragments are unsupported. Directory-only targets are
invalid. Every non-note outcome is explicit and has no note lookup keys.

`Resolve(ctx, query, set)` accepts at most four keys, eight rows, four matching keys
per row and two distinct bindings per key. Each row reports its actual queried
matches. It rejects foreign keys, inconsistent binding/path/kind evidence and
oversized sets instead of truncating them. An indexed path match wins over
fallback. Basename and alias matches form one fallback set. Deduplication uses
binding identity, so equal-content notes remain distinct. Ordered path keys are
evaluated before fallback. Incomplete evidence is always `pending`; a complete
empty set is `missing`. A selected attachment has no fabricated note binding.
The result repeats the supplied namespace generation.

`ResolveFragment(ctx, fragment, headings)` receives only the selected exact
version's ready heading list. It first tries an exact generated anchor, then a
unique match under the same normalized heading-text recipe. Duplicate text is
ambiguous; empty fragments are `none`; block IDs are `unsupported`. A ready empty
list is missing. The adapter must report pending/unavailable before calling if
that exact version's parse is absent or terminally limited. Returned heading
spans are copies. Fragment UTF-8/size and heading count always validate. Empty
or block-ID fragments do not inspect unused heading contents. Any supported
nonempty fragment validates the whole supplied heading list before returning a
hit; it is not enough for the first heading to match.

## Persist and integrate

1. Parse verified original bytes and retain their immutable version identity.
2. Call `CanonicalArtifact(ctx, note)` and hash the returned canonical JSON bytes.
   Bind the artifact to source SHA/size, `Recipe` and checksum. JSON arrays are
   canonical empty arrays, fields have fixed order and HTML is not escaped.
3. Plan each link using its referring source path. Query only those indexed keys
   in one authorized publication/revision snapshot, with `LIMIT 2` per key.
4. Supply actual matches and completeness to `Resolve`. Obtain fragment headings
   only from the selected exact content version, then call `ResolveFragment`.
5. Publish derived graph data only after the owner verifies the namespace revision
   and exact source/version bindings still match. A concurrent change invalidates
   the result and requires a rebuild.

The package owns deterministic syntax and limits through
`docbank-authored-markdown/v1` and `docbank-note-targets/v1`. Storage adapters own
current-source eligibility, parse persistence, terminal parse states, key indexes,
transactional generations, paging, namespace invalidation, backup/restore and
rebuild. This package cannot verify those contracts through opaque strings.
Current stale graphs must stay pending; historical graph resolution is explicitly
unavailable in phase one. Original-version readers remain separate authorities.

## Bounds and failures

| Resource | Fixed inclusive limit |
| --- | ---: |
| Original bytes | 4 MiB |
| Frontmatter bytes | 64 KiB |
| LF characters | 65,536 |
| Root headings / link occurrences | 4,096 / 8,192 |
| Aliases / tags | 256 each |
| Title / target / source path / key / opaque identity bytes | 4,096 each |
| Individual alias/tag bytes | 1,024 |
| YAML depth / nodes | 32 / 8,192 |
| Syntax depth / nodes | 128 / 262,144 |
| Counted work / cancellation check interval | 16,777,216 / 1,024 steps |
| Aggregate derived-string budget | 8 MiB before materialization |
| Canonical artifact bytes | 16 MiB before allocation |
| Diagnostics | 32 stable codes/spans |

Bounds apply together: being under the byte limit does not exempt input from
work, depth or output budgets. Attempted expansion and normalization reserve
strings conservatively before construction. Repeated reference destinations are
charged at every use; small inputs cannot amplify into an unbounded artifact.

Every `Parse` error returns zero `Note`. Resource failures wrap `ErrLimit` in
`*LimitError` with a fixed `Resource`, never source content. Invalid original
UTF-8/NUL wraps `ErrInvalidInput`. Malformed metadata instead has bounded
codes/spans and usable body semantics. `CanonicalArtifact` validates caller-built
values and returns no bytes on errors, including `ErrInvalidArtifact` and limits.
Malformed source paths/forms or query/heading inputs use `ErrInvalidInput`;
invalid authored target syntax is a `Kind: "invalid"` value. Malformed candidate
evidence uses `ErrInvalidCandidateSet`. Cancellation returns the context error.
No failure publishes a complete-looking prefix.
