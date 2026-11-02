// Package rrf fuses ranked retrieval legs with reciprocal rank fusion.
//
// Fusion accumulates evidence from independent legs. It is not generation
// merging: vector.Merge prefers one generation's hit and drops the overlap.
// Scores from different legs are not compared directly. Each leg contributes
// weight / (k + rank), where rank is 1-based and k is chosen by the caller.
// The constant 60 is common in the literature; this package does not apply it
// unless the caller passes it.
//
// Ties break by first appearance in leg order, then by position within that
// leg. Map iteration order is never used.
//
// Group fusion keeps every alternate member. Callers decide eligibility after
// fusion. A group contributes at most once per leg, at the rank of its first
// occurrence in that leg.
//
// FuseGroupsEvery treats each leg as a required concept and keeps only
// groups that every leg found. Different members of a group may carry the evidence for
// different legs.
package rrf
