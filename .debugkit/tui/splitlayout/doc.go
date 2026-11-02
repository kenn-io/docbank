// Package splitlayout provides the shared geometry and pane styling for
// two-pane terminal layouts: a fixed-policy list pane beside a flexible
// detail pane, under a one-line title band and above a one-line info band
// and a help footer.
//
// The package is sizing policy only. Config carries an application's
// pane constants, Geometry computes border-box pane rectangles from the
// terminal size and the caller's footer height, and PaneStyle wraps
// already-rendered content in the standard single-line border. Layout
// locking, focus dispatch, resize handling, and content rendering stay
// with the caller.
//
// Geometry performs no terminal-size rejection: callers gate rendering on
// PickLayout (or their own policy) and pass whatever dimensions they
// render at. Undersized inputs produce the configured floors, including
// pane widths that can exceed the terminal when a floor binds.
package splitlayout
