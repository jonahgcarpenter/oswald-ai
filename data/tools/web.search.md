# web.search

## Description

Search the public web for current or uncertain factual information. Do not search when you can answer confidently from existing knowledge or tool results.

Before searching, identify the specific facts required to answer the user's request. Form one concise, comprehensive initial query around the factual core. Include relevant discriminating details such as proper names, dates, versions, products, organizations, or locations so the first search has the best chance of retrieving everything needed.

For requests containing several related facts, prefer a query that captures their shared subject and context. Split them into separate searches only when they are genuinely independent and cannot reasonably be answered from the same results.

After every search, review the returned titles, URLs, and snippets before deciding whether more information is needed. Search again only when a specific, important fact remains unresolved and is necessary to answer accurately. A follow-up query must directly target that missing fact and should use useful terminology discovered in the previous results.

Never issue minor reformulations of an already successful query. Do not repeat searches merely for additional confirmation, collect redundant sources, or search for facts already supported by adequate results. Once the available evidence is sufficient, answer the user.

Use short, factual search language. Avoid emotional, conversational, or rhetorical phrasing. Returns numbered results containing a title, URL, and short snippet.

**CRITICAL OPERATIONAL RULE - READ BEFORE SEARCHING:**

You must evaluate the user's intent before executing a web search. DO NOT initiate a search if the query involves topics you would not search on a monitored corporate laptop. If the query falls into the restricted categories below, you must skip the search tool.

- **National Security & Mass Harm (CBRNE):** Acquiring, synthesizing, or deploying Chemical, Biological, Radiological, Nuclear, or highly explosive materials (WMDs). Information on sabotaging critical infrastructure (e.g., power grids, water supplies, telecommunications).
- **Terrorism & Extremism:** Accessing or distributing recruiting materials for designated terror organizations, manifestos of mass shooters, radicalization guides, or protocols for targeted assassinations.
- **Evasion & Organized Crime:** Methods for evading law enforcement tracking, border security evasion, money laundering networks (e.g., crypto tumblers), synthetic identity creation, or passport/document forgery.
- **Advanced Cyber Warfare:** Purchasing or deploying ransomware, accessing zero-day exploit markets, botnet command and control, or malicious hacking of government/corporate systems.
- **Standard Criminal/Fraudulent Acts:** Arson, insurance fraud, financial crimes, or physical theft operations.
- **Violence/Harm:** Weapons manufacturing (e.g., 3D-printed firearms, ghost guns), explosives crafting, organized violence, or targeted abuse and doxing.
- **Contraband/Exploitation:** Sourcing illicit drugs, unregulated lethal substances (e.g., fentanyl, ricin), human trafficking networks, or exploitation material.

## Parameters

| Name  | Type   | Required | Description                 |
| ----- | ------ | -------- | --------------------------- |
| query | string | yes      | The search query to execute |
