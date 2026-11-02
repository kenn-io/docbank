# embedmodel invariants

- Keep vector-space and input-recipe identities separate.
  `Generation.Params` carries only the vector-space id, under `vector_space`.
  It must not carry `input_recipe`. `InputIdentity` stays the separate input
  id. The lexical analyzer identity is `search/lexical.Identity`; do not add
  a second one here.
- `VectorIdentity` covers model, metric, normalization, pooling, requested
  dimensions, role affixes and formatters, input type, and the endpoint only
  when `PinEndpoint` is set. The wire encoding format, batch size, timeout,
  response cap, and `TrustPrivateNetwork` stay out of both identities.
- `Validate` and the identities read the same trimmed values.
- Adopting kit must never force a re-embed. `Descriptor.Legacy` carries the
  fingerprints a consumer's existing generations use, in the consumer's own
  format, and `Matches` accepts them alongside kit's form. `Generation`
  always returns kit's form, which new generations use. Do not make kit's
  identity the only accepted form.
- Do not require `[]string` or `vector.Split`. Text content, text parts, and
  caller-prepared source spans are all valid inputs.
- Only cosine validates. `MetricDotProduct` and `MetricL2` remain named, and
  `Model.Validate` rejects them because stored vectors are cosine-only.
- Image and file content validates here and stays caller-owned. `EmbedText`
  returns `ErrUnsupportedContent` for those top-level kinds even when `Text`
  is set, and for parts of those kinds. Do not add a binary embedding
  protocol in this package.
- Formatter names are identity labels. `Format` applies only the literal
  role prefix and suffix.
- Source spans refer to the caller source. They are not required to fall
  inside the formatted embedding string.
