// Package eval evaluates document retrieval recipes against versioned public
// or synthetic corpora with graded relevance judgments. A System may opt into
// an evaluator-owned rerank stage over a bounded candidate prefix. Reports
// retain hit rates, per-query latency and usage observations, and keep missing
// provider token or cost evidence unavailable instead of treating it as zero.
// A comparison built from synthetic vectors or provider fixtures is a wiring
// check and does not support a quality recommendation.
package eval
