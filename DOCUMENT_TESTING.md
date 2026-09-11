# Local Document Testing

This setup uses a separate Docker volume. It does not mount the repository's
database. Do not attach a production volume or run two instances using the same
Discord bot token. The application connects to the configured model gateway and
chat services; the parser tests below do not.

## Parser Tests

Build and run the synthetic PDF, English OCR, Office, spreadsheet, and extraction
tests using the same parser packages as the application image:

```bash
docker build --target document-tests -t oswald-document-tests:local .
docker run --rm --network=none --read-only \
  --tmpfs /tmp:rw,exec,size=1g oswald-document-tests:local
```

No application credentials or live model service are needed. The build downloads
dependencies; test execution has networking disabled. This does not establish
network isolation for production parser subprocesses.

## Gateway Testing

The Compose file loads an ignored `.env.documents` file. Use the variable names
in `.env.example`, with a test gateway identity and a test model route. Required
settings include `LLM_GATEWAY_MODEL`, `MCP_CONFIG_ENCRYPTION_KEY`, and at least one
complete gateway configuration. Generate a dedicated MCP encryption key with
`openssl rand -base64 32`; keep it stable while reusing the test database.

To reach a model gateway running on the Docker host, set
`LLM_GATEWAY_URL=http://host.docker.internal:8080` rather than `localhost`.
The host service must listen on an address reachable from Docker. The same rule
applies to BlueBubbles and other host services. Leave unrelated integrations
disabled, especially the example ComfyUI URL, unless needed for the test.

```bash
docker compose -f compose.documents-test.yml up --build -d
docker compose -f compose.documents-test.yml logs -f oswald
```

Discord needs no published inbound port. The test configuration maps Home
Assistant to host port 18000 and BlueBubbles to 18090, on loopback only, and fixes
their container ports to 8000 and 8090. A remote BlueBubbles server needs a
deliberately configured secure route to the webhook; loopback publication alone
does not expose it remotely.

The named volume persists `/home/oswald-ai/data/database`. `docker compose down`
keeps it. Do not use `down --volumes` unless you intend to erase the entire test
database. An ordinary named volume is not a filesystem quota. Keep sufficient
disk headroom for source files, extracted text, indexes, database overhead, and
WAL; the application also checks available disk before document admission.

## Acceptance Checklist

1. Upload TXT, a native-text PDF, a scanned PDF, DOCX, PPTX, and XLSX/ODS fixtures.
   Files are accepted asynchronously; the first response may report processing.
   There is no automatic completion message. Ask again or use `/documents list`.
2. Ask a natural-language question and inspect the cited document/source location.
   Ask for a whole-document summary and verify any incomplete coverage is stated.
3. Link a test Discord and iMessage identity using `/connect`. Upload on one and
   ask about the file on the other. Unlinked accounts must not see the library.
4. Restart with `docker compose -f compose.documents-test.yml restart oswald` and
   ask a follow-up. Interrupted extraction resumes after its old lease expires.
5. Check `/documents usage` and `/documents list`. Administrators automatically
   see global scope; regular users see their own libraries. Model tools remain
   private even for administrators.
6. Use `/documents forget <id>`. Try the removed ID through a follow-up request.
7. Use `/documents forget all`, inspect the explicit scope/count, then
   `/documents confirm <code>`. For administrators this selects all users'
   documents. Uploads accepted after the snapshot are not selected.
8. Run `/reset` and verify documents survive. `/memories forget all` deletes the
   user's documents as part of its broader user-data reset.
9. Test an oversized, malformed, encrypted, and unsupported file. Never use real
   private documents as regression fixtures.
10. Repeat retrieval with embeddings disabled. Canonical lexical retrieval remains
    usable. Thirty-day expiry and concurrent quota/lease races are covered by
    automated tests rather than requiring a month-long manual test.

## Limits And Privacy

Uploads allow four files, 20 MiB each, and 40 MiB total per request. Each canonical
user has 50 retained documents, 250 MiB source bytes, and 25 MiB extracted capacity.
Global limits are 2 GiB source and 200 MiB extracted capacity. Pending extraction
reserves 1 MiB per file; upload reservations also count before downloading.
Expired rows count until cleanup. Documents expire 30 days after acceptance;
reads do not extend expiry.

PDF extraction processes at most 100 pages and 25 OCR pages. Truncation and
resource-limit omissions are partial results, not a complete reading guarantee.
Legacy XLS, password unlocking, and arbitrary archives are unsupported. XLSX/ODS
cached formula values may be stale; formulas are not evaluated.

Parsers run with bounded subprocess resources and sanitized environments, but
are **not sandboxed from the application's filesystem or network**. The Compose
limits protect resources, not secrets from a compromised parser. Use trusted
synthetic files for local acceptance testing. Extracted content sent to the model
is subject to that provider's retention. Group answers can quote private library
content, and deleting a document does not erase earlier answers or backups.
