# Qualified font glyphs

`font-glyphs.json` records the complete named-glyph inventory of the twelve
PDFium standard programs accepted by `native_font.go`. It contains no font
program bytes. The inventory was extracted independently with FreeType using
`FT_LOAD_NO_SCALE | FT_LOAD_NO_HINTING | FT_LOAD_NO_BITMAP`; `Points` and
`Contours` describe each outline. There are 2,816 glyphs: 2,792 nonempty outlines
and 24 empty glyphs (`.notdef` and `space` in each program).

The program hashes in `qualifiedFontPrograms` bind this inventory to the pinned
PDFium asset in `dependencies.json`. Do not extend the accepted programs based
on a name, a few sample glyphs, or an empty render of an arbitrary font.

Run the complete finite-raster qualification after changing the PDFium asset,
accepted programs, or scratch rendering geometry:

```sh
DOCBANK_QUALIFY_FONT_PROGRAMS=1 go test -tags fts5 ./internal/pdfproduction \
  -run '^TestQualifiedFontGlyphInventory$' -count=1 -v
```

This selects every named glyph through a synthetic PDF's Differences encoding
and calls the production `blankCodes` renderer with the actual source font.
Every nonempty outline must paint nonzero alpha; every empty glyph must paint
none. The test adds no FreeType dependency. A changed program requires a fresh
independent inventory, program hashes and qualification before acceptance.
