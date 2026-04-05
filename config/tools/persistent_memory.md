# persistent_memory

## Description

Manage persistent long-term memory about the current user. This memory survives
restarts and is unique to each user across all conversations.

Use this to store, retrieve, or remove facts the user explicitly shares —
things like their name, age, location, occupation, or preferences. Never infer
or assume facts that were not stated directly.

Each remembered fact is stored as a plain statement followed by the evidence
that proves it — a direct quote from the user and the date it was stated:

    The user's age is 23.

    - Evidence: User stated "If I'm 23 how much will I have at 65". Date: [2025-12-10].

**Actions:**

- **remember** — Store or update a fact. Requires `statement` (the fact as a
  concise declarative sentence) and `evidence` (a direct quote or observation
  from this conversation that proves the statement is true). Optionally provide
  `category` to organise the fact.
- **recall** — Retrieve stored facts about the current user. Call this
  proactively at the start of a conversation to check what you already know.
  Optionally provide `category` to retrieve only that section (identity,
  preferences, context, or notes). Omit category to retrieve all stored facts.
- **forget** — Remove a specific fact by passing its exact `statement` text.
  Pass `statement = "all"` to wipe the user's entire memory profile (only do
  this if explicitly asked).

**Categories** (for the `remember` and `recall` actions):

- **identity** — name, pronouns, age, location, occupation
- **preferences** — likes, dislikes, communication style, settings
- **context** — ongoing projects, current goals, situation
- **notes** — everything else (default if omitted)

## Parameters

| Name      | Type   | Required | Description                                                                                  |
| --------- | ------ | -------- | -------------------------------------------------------------------------------------------- |
| action    | string | yes      | Operation to perform: "remember", "recall", or "forget"                                      |
| statement | string | no       | The fact as a concise declarative sentence (e.g. "The user's name is Alex"). Required for remember and forget |
| evidence  | string | no       | A direct quote or observation proving the statement (e.g. User stated "my name is Alex"). Required for remember |
| category  | string | no       | Category for the fact: identity, preferences, context, or notes. Defaults to notes. Used with remember and recall. |
