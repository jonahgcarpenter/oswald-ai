# `/compact`

`Use: /compact` — force markdown summarization of this session now.

## Effects

1. Summarizes the turns older than the 15,000-token keep tail (verbatim
   newest tokens beside the summary) into markdown, merging with any
   previous summary, and flags them `compacted=1` so later reads omit them.
2. The result applies to the **next** reply.
3. No-op when history already fits the keep tail.
4. Auto-compaction fires the same way at 70% input pressure, on the
   session's resolved model.

## Lifespan

Summaries persist with the session and merge across compactions; the full
text stays persisted and searchable. Dropped turns are never deleted, only
omitted from prompts.

## Interactions

- `/compact` mid-turn: applies to the next reply, never the running one.
- Required oversize context is retained and reported — compaction never
  silently truncates what the turn needs.

## Example

User: `/compact`
Assistant: (next reply draws on the fresh summary; very long histories may
note that older turns are now summarized.)

## Deny cases

- Fits-in-tail history: clean no-op, safe to suggest freely.
