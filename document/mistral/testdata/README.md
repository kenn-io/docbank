---
title: "Synthetic linearized PDF fixture"
description: "Provenance and structure of the synthetic linearized PDF fixture used by Mistral adapter tests."
last_edited: 2026-09-27
---
`linearized.pdf` is a synthetic, blank, one-page PDF. It was generated with
qpdf 12.3.2 using `--linearize --static-id --stream-data=uncompress`. The final
trailer repeats `/Root 3 0 R` and `/Size 7`, and `/L` reflects the new file
length. This reproduces the linearized layout accepted before this change;
qpdf's default omits `/Root` from the final trailer. The modified fixture passes
`qpdf --check`. It contains no source document or personal metadata.

Its final `startxref` points to the first-page cross-reference table near the
start of the file. The tests also remove its final `%%EOF`, leaving only the
early marker before the page objects. No qpdf installation is needed to run them.
