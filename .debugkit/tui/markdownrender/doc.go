// Package markdownrender renders Markdown for terminal display.
//
// It uses Goldmark with GFM and definition-list extensions, leaves code
// unhighlighted, and sanitizes source text through termtext. Callers should
// pass Render output through ANSIWrappedLines before writing it to a terminal.
package markdownrender
