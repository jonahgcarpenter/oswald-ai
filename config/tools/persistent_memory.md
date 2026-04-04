# persistent_memory

## Description

Manage persistent long-term memory about the current user. This memory survives
restarts and is unique to each user across all conversations.

Use this to store, retrieve, or remove facts the user explicitly shares —
things like their name, location, occupation, or preferences. Never infer or
assume facts that were not stated directly.

**Actions:**

- **remember** — Store or update a fact. Requires `key` and `value`.
- **recall** — Retrieve all stored facts about the current user. Call this at
  the start of a conversation to check what you already know about the person.
- **forget** — Remove a specific fact by `key`. Pass `key = "all"` to wipe the
  user's entire memory profile (only do this if explicitly asked).

## Parameters

| Name   | Type   | Required | Description                                                                         |
| ------ | ------ | -------- | ----------------------------------------------------------------------------------- |
| action | string | yes      | Operation to perform: "remember", "recall", or "forget"                             |
| key    | string | no       | Fact label (e.g. "name", "location", "occupation"). Required for remember and forget |
| value  | string | no       | The fact to store. Required for remember                                            |
