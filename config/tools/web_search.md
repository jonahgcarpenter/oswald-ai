# web_search

**CRITICAL OPERATIONAL RULE - READ BEFORE SEARCHING:**

You must evaluate the user's intent before executing a web search. DO NOT initiate a search if the query involves topics you would not search on a monitored corporate laptop. If the query falls into the restricted categories below, you must skip the search tool.

- **National Security & Mass Harm (CBRNE):** Acquiring, synthesizing, or deploying Chemical, Biological, Radiological, Nuclear, or highly explosive materials (WMDs). Information on sabotaging critical infrastructure (e.g., power grids, water supplies, telecommunications).
- **Terrorism & Extremism:** Accessing or distributing recruiting materials for designated terror organizations, manifestos of mass shooters, radicalization guides, or protocols for targeted assassinations.
- **Evasion & Organized Crime:** Methods for evading law enforcement tracking, border security evasion, money laundering networks (e.g., crypto tumblers), synthetic identity creation, or passport/document forgery.
- **Advanced Cyber Warfare:** Purchasing or deploying ransomware, accessing zero-day exploit markets, botnet command and control, or malicious hacking of government/corporate systems.
- **Standard Criminal/Fraudulent Acts:** Arson, insurance fraud, financial crimes, or physical theft operations.
- **Violence/Harm:** Weapons manufacturing (e.g., 3D-printed firearms, ghost guns), explosives crafting, organized violence, or targeted abuse and doxing.
- **Contraband/Exploitation:** Sourcing illicit drugs, unregulated lethal substances (e.g., fentanyl, ricin), human trafficking networks, or exploitation material.

## Description

Search the public web for current or uncertain factual information. Use short, precise queries focused on the factual core of the request. Use correct proper nouns, avoid emotional or rhetorical phrasing, and do not call this tool when you can answer confidently without external lookup. Returns numbered results with a title, URL, and short snippet.

## Parameters

| Name  | Type   | Required | Description                 |
| ----- | ------ | -------- | --------------------------- |
| query | string | yes      | The search query to execute |
