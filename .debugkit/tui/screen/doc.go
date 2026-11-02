// Package screen composes already-rendered terminal lines and screens.
// It measures terminal cells rather than bytes or runes and treats ANSI
// control sequences as zero-width data that must not be split.
//
// Overlay coordinates are zero-based: row 0, column 0 is the top-left cell of
// the declared viewport. OverlayAt clips the panel against that viewport; it
// does not move an out-of-bounds panel back on screen. Negative coordinates
// therefore display only the intersecting bottom or right portion. A panel is
// rectangular, with the width of its widest line, and shorter lines erase the
// remainder of their row in that rectangle.
//
// Width is measured in grapheme clusters using github.com/charmbracelet/x/ansi.
// A wide grapheme is atomic: if a cut crosses it, the complete grapheme is
// omitted and Splice pads any uncovered cells with spaces. The dependency's
// state machine recognizes CSI, OSC, DCS, APC, C1 controls, and modern Unicode
// width rules more completely than a small CSI/OSC-only parser.
//
// Screen geometry supports printable graphemes plus line-local SGR styling and
// OSC hyperlinks. OverlayAt splits inputs at newlines, so every input line must
// independently balance its ANSI state; opening a style or hyperlink on one
// line and closing it on another is unsupported. Tabs, carriage returns,
// backspaces, cursor movement or save/restore, erasing, clipboard operations,
// DCS, and APC are preserved as zero-width bytes, but their position-dependent
// or side-effecting behavior is not modeled. Suffix may replay zero-width
// controls encountered before a cut, so such controls can execute more than
// once. Callers must sanitize untrusted terminal content before composition.
//
// Inputs must contain valid UTF-8 and complete ANSI sequences. The helpers
// preserve, but do not validate or sanitize, control sequences; an incomplete
// or malformed sequence can cause following bytes to be interpreted as part of
// that sequence and measured as zero-width.
package screen
