# embedfit invariants

- Count tokens on the formatted string: prefix, source slice, suffix.
- Measure overlap on the source slice. The next window must start after the
  current start.
- Truncation is explicit. `reject` returns an error for a hard cut or a
  dropped tail. `drop_tail` may hard-cut, sets `Truncated` on that span, and
  sets `TailDropped` when source remains.
- Source coordinates refer to the original string. `Prepared.Text` is the
  formatted model input using the prefix and suffix Fit counted. `Prepared`
  takes no arguments so a caller cannot format with a different pair.
- A separator at the start of the preferred window is a valid soft cut.
  The text after it stays in the fit. Boundaries remain paragraph breaks,
  sentence endings, and ASCII spaces. A separator just past the window
  also counts, because it is trimmed away.
- Spans never start or end with blank text as `embedmodel.BlankText`
  defines it. Skip blank runes before counting a window, then trim the
  span's tail. The trimmed span is a prefix of the counted window, so the
  monotonic tokenizer rule keeps it in budget.
- Do not wire this package into `vector.Fill` from here. Fill's prepared
  input belongs to the bounded-fill work and should consume `Prepared`.
- A tokenizer must be monotonic: a longer string has at least as many tokens
  as any prefix of it. A non-empty, non-blank string must count as at least
  one token.
