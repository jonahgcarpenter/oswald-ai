// Package documents provides local, non-persistent extraction. No memory,
// gateway, model, or document service is started or imported by this package.
//
// # Formats
//
// UTF-8 plain text, common source extensions, Markdown, CSV/TSV, JSON, XML, and
// HTML use native parsers. CSV/TSV retain parsed row/field boundaries; XML emits
// character data, and HTML omits scripts/styles/templates without fetching
// resources or executing JavaScript. JSON and source text remain literal.
// PDF uses Poppler native text per page; pages with fewer than 40 alphanumeric
// characters are rendered sequentially and OCRed with Tesseract English.
// DOC/DOCX/ODT/RTF and PPT/PPTX/ODP use LibreOffice PDF conversion first.
// XLSX/ODS use bounded ZIP/XML parsing, never LibreOffice or formula evaluation.
// XLSX preserves sparse cell coordinates, shared/inline rich strings, booleans,
// errors, and cached formula values. Numeric values (including date serials)
// remain raw: number formats/styles are not rendered. ODS preserves paragraphs,
// typed/displayed values, repeats, and covered-cell positions. Missing formula
// caches are labeled; cached results may be stale. Drawings, comments, charts,
// and embedded files are not spreadsheet text. Legacy XLS is unsupported.
// Filename extension takes precedence; MIME is used only without an extension.
//
// # Bounds
//
// Each Extractor serializes calls with a cancelable permit. The three-minute
// timeout includes permit waiting. Input is limited to 20 MiB, returned Text to
// 1 MiB, PDF processing to 100 pages, and OCR to 25 pages. Rendered pages have
// maximum 3000-pixel edges and 9 million pixels; only one raster is retained.
// Text truncation is UTF-8 safe and sets Partial. Spreadsheets allow 100 sheets
// and 10,000 cells; CSV/TSV allow 10,000 rows. A constant-memory CSV/TSV preflight
// checks the entire input before csv.Reader allocates records: each logical
// record allows 1,024 fields and 256 KiB of raw bytes, including quoting and
// embedded newlines. Any violating record rejects the input with ErrLimit,
// including records beyond the eventual output budget. ZIPs allow 2,048 entries and 32 MiB
// total inflation, reject large members above a 200:1 compression ratio, and
// check actual size, CRC, duplicate/traversal names, encryption, and symlinks.
// XML rejects DTDs and nesting deeper than 64. Archives are never unpacked.
//
// # Subprocess Security
//
// Linux native tools run at fixed paths without a shell, with a minimal
// environment (no inherited credentials/proxies), private temporary directories,
// server-owned filenames, and a new LibreOffice profile on every conversion.
// The profile disables macros/plugins, sets highest macro security, empties
// trusted locations, blocks untrusted-referer links, and disables Writer's
// automatic link/field/chart updates. Import/export filters are fixed per format.
// LibreOffice's explicit first-use exit code 81 permits one restart with the
// same profile and deadline; other failures are not retried.
// Temporary files are removed on success, error, or cancellation. Process-group
// termination handles ordinary descendants, including after successful exit.
// prlimit enforces per-process 1 GiB address space, 150 seconds CPU, 128 file
// descriptors, zero core size, and 32 MiB per output file. Stdout is capped at
// 1 MiB and stderr discarded. A 25ms watchdog terminates jobs exceeding 64 MiB
// aggregate temporary files or 2,048 filesystem entries. This watchdog can
// overshoot; it is not a hard disk quota or a bound on unlinked open files.
//
// These measures are NOT a sandbox: no filesystem or network isolation is
// provided, escaped process sessions are not contained, and LibreOffice/importer
// vulnerabilities or external-resource behavior are not ruled out. Deploy OS
// isolation and disk quotas when accepting hostile input. Conversion can lose
// layout or content; extraction does not establish factual accuracy or trust.
// One safe INFO measurement is emitted per Extract when a logger is supplied;
// filenames, content, command output, and raw errors are never logged.
package documents
