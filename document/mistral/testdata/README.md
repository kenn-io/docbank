`linearized.pdf` is a synthetic, blank, one-page PDF. It was generated with
qpdf 12.3.2 using `--linearize --static-id --stream-data=uncompress`. The final
trailer repeats `/Root 3 0 R` and `/Size 7`, and `/L` reflects the new file
length. This reproduces the linearized layout accepted before this change;
qpdf's default omits `/Root` from the final trailer. The modified fixture passes
`qpdf --check`. It contains no source document or personal metadata.

Its final `startxref` points to the first-page cross-reference table near the
start of the file. The tests also remove its final `%%EOF`, leaving only the
early marker before the page objects. No qpdf installation is needed to run them.

`linearized-xref-stream.pdf` represents the same synthetic page with compressed
object and cross-reference streams. qpdf 12.4.2 generated it with:

```sh
qpdf --deterministic-id --object-streams=generate --linearize \
  document/mistral/testdata/linearized.pdf \
  document/mistral/testdata/linearized-xref-stream.pdf
```

It passes `qpdf --check`. The final `startxref` points to the first-page stream;
its forward `/Prev` points to the main stream, which inherits the first-page
`/Root`. Tests cover trailing scanner data and forged suffixes. Removing the
final marker also preserves format recognition; page-tree repair is a separate
contract. The fixture contains no personal data and needs no qpdf at test time.
