// Package postgres builds mapped PostgreSQL full-text and pgvector queries.
//
// Callers own the pool or transaction and execute the returned sqlquery.Query.
// Text rank is PostgreSQL ts_rank or ts_rank_cd. Those values are not BM25
// and are not comparable with scores from another backend. Vector scores are
// converted to higher-is-better; the ORDER BY clause still uses the native
// distance operator so an index can match it.
//
// Source predicates run in WHERE before LIMIT. That is PostgreSQL's placement,
// not a raw-neighbor window. A short result does not prove the index was
// exhausted. The SQL limit is a candidate bound. Apply the eligible result
// limit after hydration.
//
// This package does not segment Chinese, Japanese, or Korean text. A
// character-phrase or dictionary analyzer has to agree with the stored
// tsvector. Pass phraseto_tsquery text that uses the same spacing, or pass a
// trusted tsquery expression.
package postgres
