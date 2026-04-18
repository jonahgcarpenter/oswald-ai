# persistent_memory

## Description

Manage persistent long-term memory about the current user. This memory survives
restarts and is unique to each user across all conversations.

Use this to store, retrieve, or remove facts the user explicitly shares, OR to
record synthesized insights based on sustained, consistent patterns in the user's
prompts over time. Never make baseless assumptions; all synthesized insights
must be grounded in repeated engagements or behaviors.

Avoid using first-person (I, my, me) or second-person (you, your) pronouns in
stored statements; always refer to the individual neutrally or as "the user".

Each remembered fact is stored as a plain third-person statement followed by the
evidence that proves it.

For explicit facts, use a verbatim quote:

    The user's age is 23.

    - Evidence: User stated "If I'm 23 how much will I have at 65". Date: [2025-12-10].

For synthesized insights, use a summary of the sustained context:

    The user prioritizes high-performance networking and uses the UniFi ecosystem.

    - Evidence: Detailed discussions regarding the configuration of a UDM-SE, 10GbE networking, and WireGuard tunnels. Date: [2025-08 to 2026-04].

**Actions:**

- **remember** — Store or update a memory. Requires `statement` (the fact/insight as a
  concise, third-person declarative sentence) and `evidence` (a direct quote
  or a summary of sustained discussion that proves the statement, including the
  date or date range). Optionally provide `category` to organize the fact.
- **recall** — Retrieve stored facts about the current user. Call this
  proactively at the start of a conversation to check what you already know.
  Optionally provide `category` to retrieve only that section. Omit category
  to retrieve all stored facts.
- **forget** — Remove a specific fact by passing its exact `statement` text.
  Pass `statement = "all"` to wipe the user's entire memory profile (only do
  this if explicitly asked).

**Categories** (for the `remember` and `recall` actions):

- **identity** — Name, pronouns, age, location, occupation, and core demographic facts.
- **system_rules** — Explicit, non-negotiable instructions ("always do X", "never do Y") and corrections to AI behavior.
- **preferences** — Likes, dislikes, preferred tech stacks, communication style, and general settings.
- **notes** — Ongoing projects, completed tasks, synthesized insights, general observations, and everything else (default if omitted).

## Parameters

| Name      | Type   | Required | Description                                                                                                             |
| --------- | ------ | -------- | ----------------------------------------------------------------------------------------------------------------------- |
| action    | string | yes      | Operation to perform: "remember", "recall", or "forget".                                                                |
| statement | string | no       | The memory as a concise third-person declarative sentence. Required for remember and forget.                            |
| evidence  | string | no       | A verbatim quote OR a summary of sustained engagement proving the statement, plus the date(s). Required for remember.   |
| category  | string | no       | Category for the fact: identity, preferences, system_rules, or notes. Defaults to notes. Used with remember and recall. |
