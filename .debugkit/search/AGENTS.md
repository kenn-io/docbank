# search package invariants

Shared query construction lives under `search/`. Callers own database handles,
schema mappings, constraints, and which rows are eligible. Do not add an
execution-plan framework or an ORM.

## Analysis identities

- Literal quoting, character-phrase adjacency, and Chinese segmentation are
  different analyzer identities. Index and query preparation must match.
- Character-phrase analysis splits Han, Hiragana, Katakana, and Hangul into
  adjacent characters. It is not morphological analysis.
- Chinese segmentation is a caller-supplied segmenter. The runtime fingerprint
  covers the library and dictionary bytes. This module does not install that
  runtime. Hiragana, Katakana, and Hangul queries are not sent to it.
- CJK search is off until the caller enables it. Enabling it adds a second
  index the caller creates and names. The ordinary index stays.
- IndexFor sends a query to the CJK index when the text contains Han, Hangul,
  Hiragana, or Katakana. Other queries use the ordinary index. A mixed query
  uses the CJK index.
- `PrepareAdvanced` returns caller syntax unchanged. Do not infer it from a
  leading quote.

## Fusion

- Reciprocal rank fusion accumulates evidence from retrieval legs. It is not
  `vector.Merge`. Generation merge prefers one generation and drops the overlap.
- Fusion does not apply the final result limit and does not drop alternate
  group members. Callers check eligibility first.
- Ties follow input order. Do not range over a map to order hits.
- Leg scores are ranks, not raw distances. Do not compare BM25 with `ts_rank`
  or a vector distance by their numeric values.
- `FuseGroupsEvery` and `RunGroupsEvery` intersect legs: a group must
  appear in every leg. They still rank by reciprocal rank. A caller whose legs share one
  score scale, such as vector legs on one generation, may carry the score in
  the member and re-rank by it. Do not build that into fusion.
- An intersection is only as complete as its narrowest leg. Any full window
  can hide a group, so report it rather than calling the result exhaustive.

## Backend SQL

- SQLite FTS and sqlite-vec queries stay in their packages. PostgreSQL and
  ClickHouse builders return the same `sqlquery.Query` value.
- PostgreSQL text rank is `ts_rank` or `ts_rank_cd`. Filters run before
  `LIMIT`. That limit is a candidate bound, not proof the index is exhausted.
- ClickHouse 26.2 has no `asciiCJK` tokenizer and no `hasPhrase`. Do not
  describe `ngrams` or `splitByNonAlpha` as Chinese segmentation.
- ClickHouse HNSW is used only when `ORDER BY` is the distance function alone.
  A predicate does not widen that window: the nearest neighbor can fail the
  predicate and leave a small limit empty.
- ClickHouse statements are not SQLite transactions. Do not wrap them in
  SQLite isolation claims.
