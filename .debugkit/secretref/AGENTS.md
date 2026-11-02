# secretref invariants

- One configuration value holds a secret or names its source. A string is
  always the literal secret; it is never parsed or expanded. Every other
  source is a field of the table form (`env`, `file`). Callers declare a
  single key of type `Ref`; do not add parallel `_env` or `_file` keys.
- Add a source, such as a secret manager or a credential helper, as a new
  table field. Update `Validate`, `UnmarshalTOML`, `UnmarshalJSON`,
  `MarshalTOML`, and `Resolve` together, with tests. Never give the string
  form a new meaning.
- `Ref` must decode with any TOML library: tagged exported fields and
  `UnmarshalText` cover libraries that decode tables into structs, and
  `UnmarshalTOML` covers decoders that try `UnmarshalText` on tables. Do not
  import a TOML library here.
- A table names exactly one source. More than one, or an unknown field, is an
  error.
- An unset reference resolves successfully without a secret. A configured
  environment or file source that is missing, empty, or unreadable is an
  error. Callers decide whether to keep running without the provider.
- `Source` and `Reason` never contain the secret. They may name the variable
  or the configured path.
- Files open through `safefileio.OpenCurrentUserFile` and must pass
  `safefileio.ValidatePrivateCurrentUserFile`. Do not follow symlinks, block
  on a FIFO, or repair permissions here.
- The literal secret is also the exported `Value` field, so encoders that
  write struct fields instead of calling `MarshalTOML` still keep it; they
  write `{ value = "..." }`, which every decoder reads back.
