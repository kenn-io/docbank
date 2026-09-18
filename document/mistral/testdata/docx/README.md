# DOCX fixtures

These files contain synthetic text only. They exist for the Mistral adapter
tests and do not contain a private document corpus.

`explicit-breaks.docx` has eleven nonempty pages separated by ten explicit
`w:br` page breaks. Its `docProps/app.xml` deliberately reports one page.
The test uses the generated PDF count, so `MaxUnits=10` rejects the document
before HTTP and `MaxUnits=11` admits it when the renderer produces eleven
pages.

`realistic-word.docx` and `libreoffice.docx` are deterministic Word-shaped
packages constructed for adapter tests. The local Windows environment had no
Microsoft Word or LibreOffice when these fixtures were built. They are not
producer-fidelity evidence. Set `DOCBANK_TEST_WORD_DOCX` to a synthetic file
actually saved by Microsoft Word before running the Linux owner proof.

The packages use fixed synthetic text, fixed XML, and stored ZIP entries. The
local construction used Python's standard-library `zipfile` module. Recreate
them with the same entry content and fixed metadata before changing a digest.

| File | SHA-256 | Provenance |
| --- | --- | --- |
| `realistic-word.docx` | `96f16d4774dee3b689544a066f3bd0f2bf1bbac27590911a1f64b6e8f96f12fb` | Constructed Word-shaped adapter fixture |
| `libreoffice.docx` | `5ad9b906f72968336ef8ba662eb9e3cef06db8f282c18b1eb98b1927d32c0aa3` | Constructed LibreOffice-shaped adapter fixture |
| `explicit-breaks.docx` | `424b0bdce7337cf7e4f9c20d516f6d275856730391b8f5867a06a34cadd584df` | Constructed explicit-break adapter fixture |
