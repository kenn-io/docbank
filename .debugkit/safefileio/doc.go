// Package safefileio provides small hardened file I/O primitives for local
// runtime state that must belong to the current user.
//
// CreatePrivateFile and CreatePrivateTemp are the sanctioned way to make a
// private file: they create a new file that is private from the moment it
// exists and never repair an existing one.
package safefileio
