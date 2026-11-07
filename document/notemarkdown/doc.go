// Package notemarkdown parses authored Markdown originals and plans explicit
// note targets without I/O. Spans are exact original-byte ranges, independent
// of sanitized rendition citations. Its fixed recipes include resource limits
// and canonical artifact encoding; owner adapters supply namespace authority.
//
// Targets retain exact case and Unicode spelling. URI query/fragment syntax is
// split before one percent-decoding pass. Encoded separators and namespace
// escapes are invalid; literal interior backslashes remain filename bytes.
// Only extensionless wiki targets gain .md completion and bare-name fallback.
//
// Adapters authorize all bindings against one publication/revision snapshot
// and certify completeness for every queried key. They pass fragment headings
// from the selected exact version only. Unprocessed/stale storage state is
// pending at that boundary; this package cannot infer it from opaque IDs.
package notemarkdown
