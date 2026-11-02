// Package atomicfile writes files so readers see either the old content or
// the new content, never a partial file.
//
// WriteFile and Create stage the new content in a temporary file next to the
// target, fsync it, and rename it over the target. WriteNew publishes only
// when nothing exists at the target. Both refuse to replace a symlink or
// junction at the target unless WithFollowLink asks to write through it, and
// neither ever falls back to copying across volumes.
//
// Callers that stage a file themselves can publish it with Replace, the
// rename WriteFile and Commit use, or with RenameNoReplace. On Windows
// versions and file systems that support POSIX-semantics renames, Replace
// succeeds while readers hold the target open with FILE_SHARE_DELETE.
//
// A failure after the new content became visible at the target wraps
// ErrPublished; a failed directory sync also wraps ErrNotDurable. With
// WithoutSync no directory is synced, so the absence of ErrNotDurable then
// says nothing about durability.
//
// atomicfile assumes the target and staging directories cannot be modified by
// untrusted users. The link refusal is a check at Create and just before
// publication, not an atomic guarantee against a link installed concurrently;
// anyone able to rename entries in those directories could replace the
// published file directly anyway.
package atomicfile
