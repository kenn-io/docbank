// Package fslink provides symlink- and Windows-junction-aware filesystem
// helpers: classify links, read and create them, and open paths without
// following them.
//
// On Windows, an entry counts as a link when its reparse tag is a name
// surrogate. That covers symbolic links, directory junctions, and volume mount
// points, but not cloud-file placeholders, deduplicated files, or other
// reparse points that store data in place. Classify, IsLink, OpenFile,
// OpenRegular, and ReadFile follow that rule. The root-confined OpenInRoot and
// OpenRootNoFollow build on os.Root, which on Windows refuses every reparse
// point, so they also refuse those data-holding reparse points.
package fslink
