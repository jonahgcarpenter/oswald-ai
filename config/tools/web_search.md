# web_search

## Description

Search the web for current or factual information. Use precise, targeted queries.

**Guardrails — Do NOT search, if the user's message involves:**

- Constructing, synthesizing, or acquiring weapons, explosives, or dangerous substances
- Harming, stalking, threatening, or killing specific individuals or groups
- Child sexual abuse material or exploitation of minors
- Committing acts of terrorism or mass violence
- Synthesizing or manufacturing illegal drugs

**Query Construction Rules:**

- Extract only the factual core of the question — ignore opinions, profanity, sarcasm, and rhetorical framing
- Use correct proper nouns (band names, product names, people's full names)
- Keep queries short (3–7 words) and precise
- Never include subjective or emotional language in the query

If no web search is needed, answer directly without calling this tool.

## Parameters

| Name  | Type   | Required | Description                 |
| ----- | ------ | -------- | --------------------------- |
| query | string | yes      | The search query to execute |
