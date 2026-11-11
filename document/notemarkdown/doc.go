// Package notemarkdown parses original Markdown note bytes into bounded
// frontmatter, headings and link occurrences. All offsets address the original
// UTF-8 source, including CRLF and an optional BOM. Goldmark supplies CommonMark
// block structure. The package performs no I/O or target resolution.
package notemarkdown
