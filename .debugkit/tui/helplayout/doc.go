// Package helplayout reflows unstyled key-and-description items into rows for
// aligned terminal help and footer displays.
//
// The package makes layout decisions only. It does not style or render text
// and has no dependency on Bubble Tea, bubbles, or lipgloss. Callers remain
// responsible for rendering the returned rows with their own Charm version or
// another terminal UI stack.
//
// HelpItem values must contain single-line, unstyled, printable text. Tabs,
// newlines, control characters, and ANSI escape sequences are outside the text
// model because their rendered width depends on terminal state. Styling should
// be applied after reflow and should not change the visible item text.
//
// ReflowRows measures item text in terminal cells. ItemWidth and ColumnWidths
// expose the same measurements so callers can add alignment fill without
// remeasuring styled text. Callers pass the complete visible width inserted
// between columns, including borders, separators, or leading padding.
// Alignment fill that expands a shorter cell to its column width is already
// represented by ColumnWidths and is not part of the gap.
package helplayout
