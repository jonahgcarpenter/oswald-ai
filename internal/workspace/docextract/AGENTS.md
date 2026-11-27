# workspace/docextract/ — Inbound Document Text Extraction

Applies on top of the root `AGENTS.md` and `internal/workspace/AGENTS.md`
(never-log rules: never log file contents, paths, or extracted text).

## Shape

`extract.go` dispatches by extension and bounds input (`IsExtractable`,
`Extract`, `extractText`); `docx.go` handles Word (`extractDOCX`,
`docxParagraphs`); `xlsx.go` handles workbooks (`extractXLSX`, sheet/shared-string/row
helpers, 5000-row / 256-col caps); `pdf.go` handles PDFs (`extractPDF`,
`pdftotext` preferred with stdlib fallback, garble check, stream filters).
One topic per file; shared caps live in `extract.go`.

## Bounds that bite

Input ≤10 MiB; `.docx/.xlsx/.pdf` auto-extract to text, images refuse with a
`vision_analyze` redirect. Output is plain text for `read_file` paging;
binary bytes never enter the prompt.

## Tests

Cover docx/xlsx/pdf fixtures (synthetic files only), oversize rejection,
unsupported extensions, and `pdftotext` present vs absent paths — with local
fixtures, never operator documents.
