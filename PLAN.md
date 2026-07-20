# Oswald v4.0.0 Evolving Memory Plan

## Release Objective

Oswald v4.0.0 should be a compact, dependable multi-tenant evolving AI harness:
one shared Oswald identity, one operator-controlled soul, private memory for each
canonical user, shared factual memory about the deployment, and durable session
continuity.

The design combines the strongest practical ideas demonstrated by Hermes Agent,
OpenClaw, and similar production agent systems:

- A small, stable, high-authority base prompt.
- Automatic memory formation during ordinary conversation.
- Strict separation between agent, user, and session memory.
- Canonical state separated from rebuildable retrieval indexes.
- Confidence that evolves as independent evidence accumulates.
- Hybrid retrieval instead of injecting an unbounded memory corpus.
- Summary-plus-tail continuity for long sessions.
- Explicit provenance, tenant isolation, deletion, and audit boundaries.

This document is the release contract for v4.0.0. Historical implementation
phases and already-resolved gaps are intentionally omitted.

## Product Model

One canonical user is one tenant.

```text
One Oswald deployment
    |
    +-- Soul
    |      shared identity, personality, and operator policy
    |
    +-- Global memory
    |      shared evidence-backed facts Oswald learns about himself
    |
    +-- Canonical user A
    |      private profile, memories, sessions, and user MCP servers
    |
    +-- Canonical user B
           private profile, memories, sessions, and user MCP servers
```

This is not an organization or shared-workspace model. Personal facts never
become global merely because multiple users discuss the same subject.

## Non-Negotiable Principles

1. `data/memory/soul/soul.md` is the base system prompt.
2. The soul is not model-readable or model-writable through tools.
3. Soul changes are operator deployment operations performed outside the agent
   loop, with filesystem and deployment access providing authorization.
4. User memory is formed automatically during normal conversation. A user does
   not need to say "remember this" or invoke a command.
5. An explicit remember or correction request is evidence of intent and may
   increase confidence or importance, but it is not a prerequisite for saving.
6. Global memory contains facts Oswald learns about himself, his implementation,
   deployment, version, architecture, integrations, and capabilities.
7. Global claims without hard evidence may be retained at low confidence. They
   must remain visibly uncertain and cannot silently become policy or capability
   authorization.
8. Every memory records its scope, evidence, confidence, provenance, source
   authority, claim identity, lifecycle state, and timestamps.
9. Memory is lower-authority reference data. No memory can grant permissions,
   expose tools, alter authorization, or override the soul.
10. SQLite is canonical. FTS, vectors, rendered profiles, and caches are derived
    and rebuildable.

## Memory Scopes

### 1. Soul

Purpose:

- Define Oswald's identity, personality, values, operator policy, and standing
  behavior.

Contract:

- Loaded fresh as the first system content on every request.
- Never exposed through `soul.read`, `soul.patch`, or replacement model tools.
- Never included in tool results, privacy exports, tenant memory, summaries, or
  retrieval indexes.
- Changed only by an operator editing the deployment artifact.
- Gateway-specific trusted runtime instructions may follow the soul in the same
  system-authority layer, but memory never does.

Required v4.0.0 change:

- Delete the `soul.read` and `soul.patch` schemas, handlers, registration, tests,
  documentation, and model-facing catalog entries.

### 2. Global Memory

Purpose:

- Store factual knowledge Oswald learns about himself and this deployment.

Examples:

- Oswald is implemented in Go.
- This deployment is version 4.0.0.
- Oswald supports Discord, iMessage, and WebSocket gateways.
- A configured integration has a particular verified capability.

Contract:

- Shared across every tenant.
- Stored separately from the soul and from user memory.
- Injected or recalled only as explicitly lower-authority factual context.
- Read fresh so corrections can affect the next request without resetting a
  tenant session.
- Supports confidence reinforcement, contradiction, supersession, forgetting,
  and evidence inspection.
- Never stores credentials, tokens, private keys, authorization headers, raw
  tool payloads, or executable instructions.

### 3. User Memory

Purpose:

- Learn stable facts, preferences, relationships, projects, commitments, and
  environment details about one canonical user.

Contract:

- Strictly tenant-scoped by authenticated canonical user.
- Built automatically after successfully delivered ordinary turns.
- Direct facts may use the smallest exact first-person evidence span from a
  longer prompt.
- Model inference must cite the complete user turn and use cautious language.
- Explicit remember and correction wording strengthens evidence but does not
  select a separate storage path.
- Stable, high-confidence identity and preference facts may enter a bounded
  session-frozen tenant profile.
- Deeper facts are supplied by query-relevant hybrid recall.

### 4. Session Memory

Purpose:

- Preserve what happened in a specific conversation without converting every
  exchange into a durable fact.

Contract:

- Scoped by canonical user, session, and generation.
- Stores completed user/assistant exchanges only.
- Becomes transcript-searchable and compaction-eligible only after delivery.
- Uses structured summaries plus a recent verbatim role-correct tail.
- Never persists intermediate reasoning or raw tool results.

## Final Tool Contract

Tool names must make scope obvious. Because v4.0.0 has not been published, use
the final names directly and do not add compatibility aliases for the current
experimental names.

### User Memory Tools

- `user_memory_save` - save or correct a user fact deliberately. This is an
  optional strengthening path, not the normal formation mechanism.
- `user_memory_search` - run deeper hybrid search over the current tenant's
  active durable memories.
- `user_memory_list` - inspect the current tenant's active memories.
- `user_memory_forget` - forget one exact current-tenant memory.

Rules:

- Handlers derive the owner from authenticated request context.
- No tool accepts a canonical user ID.
- `user_memory_save` may raise confidence for explicit intent, but automatic
  post-turn extraction uses the same canonical candidate and publication model.

### Global Memory Tools

- `global_memory_save` - propose or reinforce a fact about Oswald using current
  evidence and a stable claim identity.
- `global_memory_search` - search active global facts and their confidence and
  provenance.
- `global_memory_list` - inspect bounded active global facts.
- `global_memory_forget` - administrator-only removal of one exact global fact.

Rules:

- A model may call `global_memory_save` in any tenant's request.
- The server, not the model, resolves and validates evidence provenance.
- Same-request successful global MCP results are eligible evidence.
- User-owned MCP results can never become global evidence.
- An authenticated administrator's direct current-turn statement is eligible
  evidence.
- Ordinary user assertions about Oswald may form only low-confidence unverified
  candidates. They cannot directly establish capabilities, versions,
  authorization facts, or policy.
- Discovery catalogs, recalled memory, session summaries, assistant output, and
  prior-turn tool annotations are not evidence.
- Search/list may be available to all tenants because active global facts are
  shared. Forget and destructive correction require administrator authority.

### Session Tool

- Keep `transcript.search` separate from both memory families. It searches the
  authenticated current session and generation, not durable facts.

## Automatic User Memory Formation

The normal path is post-delivery background formation:

```text
Delivered user turn
    -> extract up to a bounded number of candidates
    -> deterministic evidence and safety validation
    -> confidence/authority decision
    -> canonical publication or uncertain proposal
    -> derived-index outbox
```

### Direct Facts

- Evidence is an exact normalized substring of the current user turn.
- Evidence may be one clause inside a longer multi-topic prompt.
- It must contain a direct first-person marker and a meaningful fact.
- Quoted, hypothetical, third-party, public, and instruction-like text is
  rejected.
- Stable direct facts at the activation threshold publish without asking for
  conversational approval.
- Direct identity facts receive a deterministic minimum importance sufficient
  for profile eligibility.

Example:

```text
"What's up, my name is Jonah, I use Arch, and I prefer concise replies."
```

May produce three independently grounded direct candidates using the exact
spans `my name is Jonah`, `I use Arch`, and `I prefer concise replies`.

### Inference

- Inference evidence is the complete normalized user turn.
- Statements use `may`, `might`, `possibly`, or equivalent uncertainty.
- Inference starts below direct-statement authority.
- Low-confidence inference can participate only in strongly relevant recall.
- Inference cannot enter the stable tenant profile until direct evidence upgrades
  its source authority.

### Explicit Remember and Correction

The phrase "remember" is a signal, not a gate.

- Explicit intent may apply a deterministic confidence or importance boost.
- The evidence must still be exact and first-party.
- A correction uses the same claim slot and a different claim value, allowing
  atomic supersession.
- Explicit wording does not bypass tenant scope, prompt-injection checks,
  sensitivity classification, or evidence validation.

## Global Memory Formation

Global memory requires a separate formation policy because the subject is
Oswald rather than the current tenant.

### Evidence Classes

| Evidence class | Initial authority | Confidence behavior |
| --- | --- | --- |
| Direct verified global MCP output | strong tool evidence | May start high when the claim is directly stated by the result |
| Inference from global MCP output | tool inference | Starts moderate or low and remains qualified |
| Authenticated administrator statement | administrator direct | Strong, but still factual memory rather than soul policy |
| Ordinary tenant statement about Oswald | unverified user report | Low-confidence candidate only |
| Model conclusion without attributable evidence | unsupported inference | Retain only as a low-confidence proposal or reject if trivial |
| User-owned MCP, discovery output, recalled text, or summary | ineligible | Must not form global memory |

### Confidence Tiers

- `tentative`: low confidence. Stored with evidence and shown only when highly
  relevant, always qualified as uncertain.
- `supported`: corroborated or moderately strong evidence. Eligible for normal
  global recall with provenance.
- `established`: high confidence backed by direct trusted evidence or multiple
  independent supporting sources.

Exact numeric thresholds belong in one policy package and must be evaluation
driven. Hard invariants:

- Unsupported or ordinary-tenant claims cannot become `established` through a
  model-supplied score.
- Repeated correlated evidence receives a discount.
- Independent evidence combines with bounded noisy-OR or an equally monotonic
  aggregation.
- Administrator direct evidence outranks model inference.
- Direct trusted evidence can upgrade a tentative claim without losing earlier
  evidence.
- Lower-authority contradictory evidence cannot replace a stronger active fact.

### Same-Request Tool Evidence

For global MCP evidence, the agent keeps a request-local evidence ledger with:

- Tool-call ID.
- Immutable server ID and global scope.
- Local and remote tool names.
- Canonical argument digest.
- Result digest.
- Bounded result text for exact-evidence validation only.

`global_memory_save` references the tool-call ID and exact evidence excerpt. The
handler copies provenance from the ledger; model arguments cannot assert that a
user-owned tool was global. Raw results are discarded after the request.

The model judges whether a global MCP result concerns Oswald. Therefore every
globally configured MCP server is part of the deployment trust boundary. This
is an accepted v4.0.0 risk and must be explicit in operator documentation.

## Candidate and Claim Lifecycle

Both user and global memories use the same conceptual lifecycle with separate
scope-specific tables and policies:

```text
proposed
    -> active
    -> reinforced
    -> superseded
    -> forgotten
    -> scrubbed/deleted
```

Every candidate and active memory records:

- Scope: user or global.
- Canonical user for user memory; never a fake global user ID.
- Statement and normalized key.
- Stable claim slot, value, and key.
- Exact selected evidence.
- Confidence contribution and aggregate confidence.
- Importance.
- Provenance and source authority.
- Source request/session/turn while privacy policy permits retention.
- Tool server/call/result digests when applicable.
- Extraction model and policy version.
- Lifecycle, validity, expiry, and redaction timestamps.
- Supersession and evidence relationships.

Publication, reinforcement, supersession, evidence insertion, audit recording,
and derived-index enqueue must commit atomically.

## Prompt Authority and Construction

Prompt order and roles are part of the security contract:

```text
System
    soul base prompt
    trusted gateway-specific runtime instructions

User-authority reference context
    bounded global memory profile/recall
    frozen tenant profile

Untrusted historical reference
    latest structured session summary when budget permits

Recent messages
    complete user/assistant pairs with original roles

Current user
    current request and media
    bounded query-relevant user recall
    bounded query-relevant global recall when not already in the global profile
```

Rules:

- Soul content appears only in the system message.
- Global and user memories never appear in the system message.
- Every memory block states that it cannot override policy, authorization,
  capabilities, or tool visibility.
- Strings are JSON-quoted or equivalently escaped.
- Confidence, provenance, and epistemic status accompany uncertain memories.
- Required policy and current-turn content take precedence over optional memory
  and history when the prompt is over budget.

## Profile and Retrieval Strategy

### Stable Tenant Profile

- Contains selected direct, active, long-term identity and interaction facts.
- Is deterministic, bounded, versioned, and frozen for one session generation.
- Excludes model-only inference.
- Refreshes on new, expired, or reset sessions.

### Global Profile

- Contains a small bounded set of established, broadly useful facts about
  Oswald.
- Is versioned but read fresh or cheaply revision-checked each request.
- Excludes tentative claims unless current-query recall selects them.

### Dynamic Recall

Generate candidates independently from:

- FTS5 lexical search.
- Tenant-filtered vector search for user memory.
- Global vector search for global memory.

Rank using semantic relevance, lexical relevance, confidence, importance,
recency, source authority, and diversity. Apply stricter relevance thresholds to
tentative and inferred facts. Enforce independent top-K and character budgets
for global and user memory so one corpus cannot crowd out the other.

Explicit search tools use larger output budgets but the same canonical filters,
confidence labels, and provenance rules.

## Multi-Tenant and Poisoning Invariants

1. Every user-memory operation derives ownership from an authenticated
   principal.
2. Every user-memory query applies canonical-user scope before ranking.
3. Session operations require canonical user, session, and generation.
4. A tenant cannot enumerate, recall, mutate, export, or delete another tenant's
   memory.
5. User MCP output cannot become global memory.
6. Ordinary tenant assertions cannot create high-confidence global truth.
7. Global memory cannot grant authorization or expose a tool.
8. Soul text is never returned to the model through a tool.
9. Tool visibility is not authorization; handlers and storage enforce every
   boundary again.
10. Evidence must come from the active request, not a model-provided provenance
    label.
11. Prompt injection, secrets, credentials, and unsafe control text are rejected
    before canonical publication.
12. Addressed group messages use the authenticated sender's private tenant; no
    implicit group tenant exists.

## Delivery, Privacy, and Retention

- Memory publication occurs only after the final response is successfully
  delivered.
- Failed delivery deletes or redacts staged user/global proposals and evidence.
- User privacy export covers user memories, candidates, evidence, profiles,
  sessions, summaries, and user MCP metadata.
- User deletion erases personal source links from global evidence without
  deleting independently useful global facts.
- Published global provenance must not retain a tenant identifier longer than
  needed for delivery fencing and abuse investigation.
- Forget removes serving copies immediately.
- Hard deletion scrubs canonical content, evidence, derived indexes, profile
  copies, and linked source material according to retention policy.
- Global destructive operations require authenticated administrator authority.
- External backups and logs remain an operator retention responsibility.

## Storage and Package Boundaries

Before v4.0.0, converge on explicit domain names:

- `internal/usermemory/` or the existing user-memory package owns tenant memory.
- `internal/globalmemory/` owns global candidates, evidence, publication,
  retrieval, and prompt rendering.
- `internal/memoryformation/` contains shared pure evidence/confidence primitives
  plus scope-specific policy entry points.
- Do not leave global memory implemented as an incidental extension of a type
  named `usermemory.Store`.

Canonical tables should be scope-explicit:

- User: existing `memory_candidates`, `memory_entries`, `memory_evidence`, and
  profile/session tables.
- Global: `global_memory_candidates`, `global_memory_entries`,
  `global_memory_evidence`, and global index metadata.

Because the current global-memory schema and names are unshipped, replace them
cleanly before freezing the v4.0.0 migration. Do not preserve experimental
`deployment_memory_*`, `memory.*`, or soul-tool compatibility aliases.

## Observability and Evaluation

Add aggregate, privacy-safe metrics for each scope:

- Candidate, activation, rejection, reinforcement, and supersession counts.
- Confidence-tier distributions and evidence counts.
- Automatic formation latency and terminal/retry outcomes.
- Lexical, vector, merged, selected, and below-threshold recall counts.
- Empty recall and irrelevant recall rates.
- Profile sizes and revisions.
- Global-memory proposals by evidence authority.
- Rejected cross-scope and invalid-provenance attempts.
- Index coverage, rebuild status, and degraded-channel counts.

The v4.0.0 evaluation corpus must include:

- Multiple direct user facts embedded in one long prompt.
- Explicit remember wording versus equivalent ordinary wording.
- Direct corrections and weaker contradictory evidence.
- Cautious inference upgraded later by direct evidence.
- Two tenants discussing similar facts without leakage.
- A non-admin triggering a global GitHub MCP result that establishes Oswald is
  written in Go.
- A user MCP producing the same result and being rejected for global memory.
- An ordinary tenant falsely asserting Oswald's version and receiving only a
  tentative global candidate.
- Administrator and trusted-tool evidence upgrading or correcting that claim.
- Prompt-injection and secret-bearing evidence rejection.
- Failed response delivery producing no active memory.
- Account deletion removing private provenance without erasing independent
  global facts.
- Global and user index degradation without cross-scope fallback leakage.

## v4.0.0 Work Plan

### 1. Freeze Vocabulary and Authority

- Remove soul tools.
- Rename model tools to the final `user_memory_*` and `global_memory_*` names.
- Rename deployment-memory code and schema to global-memory terminology.
- Centralize scope, provenance, authority, epistemic-status, and confidence-tier
  enums.

Exit gate:

- Tool schemas and package/table names make every memory scope unambiguous.
- No soul content is model-accessible through tools.

### 2. Complete Automatic User Formation

- Use direct exact spans from longer prompts.
- Keep inference whole-turn and explicitly uncertain.
- Treat remember/correction wording as stronger evidence, not a requirement.
- Consolidate equivalent claims and apply monotonic supersession.
- Ensure direct identity and durable preferences enter future tenant profiles.

Exit gate:

- Ordinary conversation creates useful user memory without commands or special
  wording.

### 3. Complete Evolving Global Formation

- Accept direct and inferred facts about Oswald with evidence-authority caps.
- Permit low-confidence unsupported or ordinary-user reports without treating
  them as truth.
- Preserve same-request global MCP provenance.
- Add independent reinforcement and authority-aware contradiction handling.
- Add global search, list, and administrator forget handlers.

Exit gate:

- Oswald can learn a codebase fact while serving a non-admin, retain uncertain
  self-knowledge safely, and improve confidence when proof appears.

### 4. Finish Prompt and Retrieval Integration

- Add bounded established global profile rendering.
- Add hybrid global recall with uncertainty thresholds.
- Keep tenant profile frozen and tenant/global dynamic recall independently
  budgeted.
- Verify memory never enters system authority.

Exit gate:

- Relevant global and private facts appear with correct authority, provenance,
  and confidence without excessive prompt growth.

### 5. Complete Lifecycle, Privacy, and Operations

- Make candidate/evidence/publication transactions atomic.
- Add global retention, forget, correction, and consistency maintenance.
- Add derived-index outbox and shadow rebuild support for global retrieval.
- Scrub tenant identity from published global provenance according to policy.
- Update privacy deletion, account merge, maintenance, and operator inspection.

Exit gate:

- No failed write, delivery, deletion, or index rebuild leaves inconsistent or
  cross-tenant serving state.

### 6. Release Validation

- Run all unit and integration tests with `sqlite_fts5`.
- Run focused race tests for formation, publication, account merge, privacy,
  global evidence, agent loops, and gateway delivery.
- Build and test migration from the last published schema.
- Test restart/idempotency and foreign-key integrity.
- Run the memory evaluation corpus and record results.
- Update `README.md`, `AGENTS.md`, tool docs, and the canonical configuration
  inventory.

Exit gate:

- Every acceptance criterion below is demonstrated by an automated test or a
  documented release check.

## v4.0.0 Acceptance Criteria

Oswald v4.0.0 is ready when:

- The soul is the base system prompt and no soul tools exist.
- User memory forms automatically from useful facts inside ordinary long
  messages.
- Explicit remember wording strengthens formation but is not required.
- User memory remains private across gateways, tenants, search, indexes,
  profiles, summaries, exports, and deletion.
- Oswald can retain low-confidence facts about himself and clearly treat them as
  uncertain.
- Hard global evidence raises confidence monotonically and preserves provenance.
- Any tenant can trigger learning from a global MCP result, while user MCP
  output cannot affect global memory.
- Ordinary tenant text cannot establish high-confidence global capabilities,
  versions, policy, or authorization.
- `user_memory_*` and `global_memory_*` tools enforce scope server-side.
- Global and user memory are lower-authority than the soul.
- Stable profiles remain bounded; dynamic recall remains relevant and bounded.
- Long sessions preserve summaries, recent role-correct turns, and searchable
  exact history.
- Publication is delivery-gated and transactional.
- Confidence, contradiction, reinforcement, forgetting, privacy, and retention
  behavior are covered by tests.
- The complete tagged test suite and focused race suite pass without live
  external services.

## Out of Scope for v4.0.0

- Organization or shared-workspace tenants.
- Arbitrary tenant-created shared memory scopes.
- Autonomous soul rewriting.
- Treating memory as executable policy.
- Persisting raw MCP result archives.
- A general memory-provider plugin ecosystem.
- Dreaming or unconstrained background self-modification.

## Final Direction

The v4.0.0 harness should be understandable as:

```text
Operator-controlled soul
    +
Evidence-evolving global self-knowledge
    +
One private evolving memory corpus per canonical user
    +
One ordered, compactable history per user session
    +
Bounded profiles and hybrid recall
    +
Explicit confidence, provenance, privacy, and lifecycle controls
```

This preserves Oswald's compact Go and SQLite foundation while giving it the
memory quality, continuity, safety, and inspectability expected from a leading
multi-tenant evolving agent harness.
