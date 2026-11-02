// Package helprender renders helplayout rows as a styled Lip Gloss table
// for aligned terminal help and footer displays.
//
// The package owns the fixed border geometry: a ▕ separator cell plus one
// padding cell before every column after the first, exported as ColumnGap.
// Callers reflow with helplayout.ReflowRows(rows, width, ColumnGap) and
// pass the result to RenderHelpTable, so height calculations and rendering
// consume the same layout.
//
// Text and border styling is supplied per call through Styles. Styles must
// not change visible text or its terminal-cell width; helplayout measures
// the unstyled text and RenderHelpTable sizes cells from those widths.
package helprender
