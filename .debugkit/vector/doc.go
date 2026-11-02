// Package vector provides backend-neutral building blocks for embedding
// content and searching the resulting vectors.
//
// It is organized in three layers:
//
//   - Transforms and value types: Split windows content into chunks,
//     PreparedChunk carries caller-built inputs and source spans,
//     Generation identifies an embedding model, EncodeBatched batches
//     encode calls, and RollupByDocument and Merge reduce and combine
//     search results across generations. These are pure functions.
//
//   - The Store contract: Store[K, G] is the persistence interface the
//     flows depend on. Implementations are a function of the caller's
//     source system and own all backend SQL and query construction; see
//     the sqlitevec subpackage for a worked example.
//
//   - Flows: Fill runs the scan-and-fill embedding loop and Search runs
//     the cross-generation query-and-merge, both over a Store.
//
// Nothing in this package opens a database, holds an index, or constructs
// backend SQL — the flows delegate every storage operation to the Store.
// Document identity is the caller's own key type K, and generation
// identity its type G; the package compares both for equality but never
// interprets them.
package vector
