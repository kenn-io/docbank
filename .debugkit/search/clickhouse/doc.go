// Package clickhouse builds mapped ClickHouse text and vector queries.
//
// Text predicates use hasToken, hasAnyTokens, hasAllTokens, or positionUTF8.
// ClickHouse 26.2.19's system.tokenizers lists splitByNonAlpha, splitByString,
// ngrams, sparseGrams, array, and bloom-filter variants. It does not list
// asciiCJK, and hasPhrase is not available. splitByNonAlpha keeps a CJK run
// as one token, so hasToken('かな') does not match 'かなを探します'.
// MatchSubstring uses positionUTF8 and matches that sequence in order.
// ngrams(1) with hasAllTokens matches characters without regard to order.
// Neither is cppjieba segmentation or a BM25 rank. Text rows are ordered by
// the source key. Do not treat that order as relevance.
//
// Vector queries use the native distance function in ORDER BY so an HNSW
// index can match. Cosine and L2 scores are higher-is-better conversions of
// distances that sort ascending. Dot-product score is the native value and
// sorts descending. CandidateLimit is the SQL LIMIT. A full window is not
// exhaustion. On ClickHouse 26.2 the HNSW index is used only when ORDER BY
// is the distance function alone, so equal distances have no key tie-break.
// A predicate does not widen that candidate window: the nearest neighbor can
// fail the predicate and leave the limit empty. ClickHouse has no SQLite-style
// transaction around this SELECT.
//
// Probes sets hnsw_candidate_list_size_for_search when positive. That setting
// is operational and is not part of an analyzer or vector-space identity.
package clickhouse
