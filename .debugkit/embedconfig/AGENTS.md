# embedconfig invariants

- Keep model, role, and input settings in their own types. Do not collapse
  them into one flat configuration struct of scalars.
- Do not default dimensions or truncation. Those choices change
  compatibility.
- `Model.Validate` accepts cosine only. `MetricDotProduct` and `MetricL2`
  stay declared and fail validation until those distances can be stored.
- `InputLimits.MaxSpans` of zero means no span cap. It is valid. It does not
  mean the token window is unset.
- `ApplyDefaults` may fill batch size 32, transport timeout 30s, and a 32 MiB
  response cap. It must not invent a model, a dimension, or a chunk limit.
- Identities live on `embedmodel.Descriptor`, not here. Keep query-side
  policy such as retrieval budgets and generation serving out of this
  package until a kit package consumes it.
- API keys are not fields of the value types (`Model`, `Roles`, `Deployment`,
  `Batch`, `Transport`). Callers resolve secrets and pass them to the HTTP
  client. `Embedder` is the configuration-file shape; its single `api_key`
  is a `secretref.Ref` (a literal string or a one-source table), and
  `ResolveAPIKey` reads it. Do not add separate keys for other secret
  sources; extend `secretref` instead.
- `Embedder` owns the standard TOML keys for one embedding endpoint. Every
  application decodes this type instead of declaring its own keys. Add a key
  here only when it is endpoint configuration any application can use; an
  application-specific setting belongs beside the `Embedder`, not inside it.
- `Embedder.Parts` fills only operational defaults (batch size, timeout). It
  requires `dims`, fixes cosine and L2 normalization, and leaves
  `Deployment.PinEndpoint` to the application's identity choice.
- `CanonicalEndpoint` and `Origin` reject an empty hostname, including a
  port with no host. An IPv6 zone is not lowercased; its percent signs are
  encoded as `%25`. Link-local checks use the address without the zone.
- `vector.Split` remains a rune window helper. Token limits here do not
  require that splitter.
