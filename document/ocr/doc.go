// Package ocr defines the provider-neutral document OCR boundary.
//
// Providers own transport, authorization, and input handling. Successful
// processors return both transient source Markdown and the deterministic
// normalized document. Applications should persist only the normalized form.
package ocr
