// Package termtext provides terminal-safe text sanitization and display-cell
// measurement helpers for terminal UIs.
//
// Sanitization belongs at a human-facing terminal boundary. Callers that
// serialize, persist, or otherwise process source text should retain the
// original bytes instead. SanitizeBlock and SanitizeLine remove all terminal
// escape sequences; SanitizeStyledBlock is only for text already styled by a
// trusted renderer and preserves SGR color and attribute sequences while
// removing other terminal commands.
//
// The package intentionally does not choose application wrap widths, render
// Markdown, expand tabs, detect source encodings, or define truncation tails.
// Those are product presentation or ingestion policies. Width operations treat
// their input as a single terminal line; sanitize or split untrusted blocks
// before measuring them.
package termtext
