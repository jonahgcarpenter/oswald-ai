# Oswald AI - Uncensored Digital Servant

> Fully local, fully uncensored, zero costly API dependencies.

## Overview

Oswald AI is a local-first, self-hosted assistant that brings your chosen language model to iMessage, Discord, WebSocket, and Home Assistant.
It combines tools, private long-term memory, conversation continuity, image understanding, and connected services into one assistant that follows you across linked accounts while keeping you in control of your data.

## Features

- Chat through iMessage, Discord, WebSocket, or the [Home Assistant integration](https://github.com/jonahgcarpenter/has-oswald-conversation)
- Send text, images, animated GIFs, and replies with quoted context
- Search the web, check the current time, and use connected MCP tools
- Remember your preferences, projects, and other useful details across conversations
- Keep continuity in long conversations and search earlier conversation details
- Link your accounts so your personal memory follows you across gateways
- Inspect, export, forget, or delete your stored memories and account data

## Memory

Oswald’s memory keeps useful context without treating every conversation detail as permanent.

- The operator-managed soul file defines Oswald’s shared personality, behavior, and standing policy. It is used as the system prompt and can only be changed by manually editing `data/memory/soul/soul.md` outside Oswald.
- Global memory stores administrator-curated facts about Oswald's implementation, hardware, deployment, version, architecture, and capabilities. These facts are shared with authenticated users but are not injected automatically; the model searches them when a question needs that information.
- Personal memory stores private details such as your preferences, projects, relationships, and environment. Relevant memories are recalled automatically and follow you across linked accounts.
- Conversation memory preserves recent exchanges and summarizes longer conversations. Oswald can search earlier details from the current conversation when needed.

Oswald automatically extracts useful direct facts from unambiguous exact first-person clauses, including multiple facts embedded in a longer message, and also forms cautiously qualified hypotheses from indirect signals. User-memory saving is handled only by always-on, post-delivery background extraction; the primary agent can retrieve personal memory but cannot save or forget it through model tools. Background model work runs only while the foreground request broker is idle and is preempted when new user work arrives. Direct evidence must begin in first person and express a positive, current, non-modal fact. Exact model wording is retained when grounded; otherwise a safe exact-evidence statement is generated without relaxing quote, question, negation, obsolete wording, condition, attribution, instruction, or claim-value checks. Sound candidates below confidence `0.35` remain proposed, sound candidates at or above it are approved, and unsound candidates are rejected regardless of confidence. Session compaction applies the same policy and can publish approved candidates under its live job lease. Conflicts, publication, expiry, deletion, and retention are tracked separately from policy state. A relationship name is eligible only when explicitly phrased as `is named` or `name is` with a compatible relationship identity slot. Every published memory retains confidence, provenance, evidence, and sensitivity metadata. Inferred memories remain explicitly uncertain and do not enter the always-present user profile until direct evidence supports them.

Administrators manage the canonical `global_memories` table explicitly with `/global-memory add <memory text>`, `/global-memory list [page]`, and `/global-memory forget <id>`. Adding an exact normalized duplicate is rejected, and forgetting a global memory permanently deletes it. For authenticated tenants, the model can only read this shared store through `global_memory_search`; it cannot add, list, or forget global memory through model tools. Search combines full-text and vector retrieval when embeddings are configured through `LLM_GATEWAY_EMBEDDING_MODEL`, degrades either channel independently, and uses a bounded canonical fallback if both indexes are unavailable.
`/reset` starts a fresh conversation without deleting personal memory. You can also inspect, export, forget, or delete retained data through the `/privacy` commands.

## Usage

### Discord/iMessage Bot

In DMs or direct chats, send any message:

```text
What is the current weather?
```

In server channels or group chats, mention Oswald:

```text
@Oswald What is the capital of France?
```

You can also reply to a message and mention Oswald to include that message as context:

```text
[Replying to Jonah: "The capital of the US is New York"]
@Oswald Is this true?
```

Replies to Oswald do not need another mention:

```text
[Reply to Oswald's message]
Can you elaborate on that?
```

### WebSocket API

The WebSocket gateway supports command-line clients, [Home Assistant](https://github.com/jonahgcarpenter/has-oswald-conversation), and other service integrations. It accepts plain-text prompts or JSON messages containing text and images. Clients obtain a 15-minute access token and a rotating refresh token through device authorization.

```bash
# Request a device code.
curl -sS http://127.0.0.1:8000/auth/device \
  -H 'Content-Type: application/json' \
  -d '{"client_name":"Laptop"}'

# Approve the returned user_code from an authenticated Oswald conversation:
# /client approve ABCD-EFGH

# Poll no faster than the returned interval.
curl -sS http://127.0.0.1:8000/auth/token \
  -H 'Content-Type: application/json' \
  -d '{"grant_type":"device_code","device_code":"<device_code>"}'

websocat \
  -H="Authorization: Bearer <access_token>" \
  ws://127.0.0.1:8000/ws
```

Store the returned refresh token securely. Exchange it with `grant_type` set to `refresh_token` before the access token expires; every successful refresh returns a replacement refresh token. Revoke a client with `/client revoke <client_id>` or `POST /auth/revoke` using its refresh token.

On a fresh database, Oswald creates a temporary administrator and prints a 15-minute access token to the terminal. Use that connection to run `/bootstrap admin <user_code> <display_name>`, connect the approved permanent administrator, and then delete the printed temporary user with `/deleteuser <canonical_id>`.

After connecting, enter a prompt:

```txt
What is Bitcoins current price?
```

Oswald sends typed streaming events followed by the final response:

```json
{"type":"content","text":"Bitcoin is currently..."}
{"model":"<gateway-route-or-model>","response":"..."}
```

## Commands

Commands are gateway-level slash commands. They are handled before requests reach the model.

### User Commands

| Command | Usage | Description |
| --- | --- | --- |
| `/help` | `/help [command]` | List available commands or show usage for one command. |
| `/connect` | `/connect [code\|cancel]` | Create, confirm, or cancel a 10-minute account-link code. |
| `/disconnect` | `/disconnect [account_number]` | List or disconnect linked accounts. The final account cannot be removed. |
| `/reset` | `/reset` | Clear the current conversation history and load the latest user profile. |
| `/privacy` | `/privacy <operation>` | Inspect, export, forget, or delete your retained data. |
| `/client` | `/client approve <code>`, `/client approve-new <code> <display_name>`, `/client list`, `/client revoke <client_id>` | Approve and manage WebSocket clients. |
| `/mcp servers` | `/mcp servers` | List your user-scoped MCP servers. |
| `/mcp add` | `/mcp add <name> <https-url> [auth-bearer=<token>] [header:<name>=<value>]` | Add or update a user-scoped MCP server. URLs and headers are encrypted at rest. |
| `/mcp remove` | `/mcp remove <name>` | Remove one of your MCP servers. |
| `/mcp enable` | `/mcp enable <name>` | Enable one of your MCP servers. |
| `/mcp disable` | `/mcp disable <name>` | Disable one of your MCP servers. |
| `/mcp test` | `/mcp test <name>` | Connect to one of your MCP servers and report its tool count. |

`/connect`, `/disconnect`, `/client`, and every `/privacy` operation require an authenticated direct conversation. `/bootstrap` additionally requires the temporary WebSocket bootstrap client. In Discord servers and iMessage groups, slash commands must mention Oswald. MCP commands can contain credentials, so use `/mcp add` only in a private conversation.

### Privacy Commands

| Command | Description |
| --- | --- |
| `/privacy inspect [memories\|candidates\|sessions\|all] [page]` | List retained record metadata and stable IDs. |
| `/privacy export` | Export your retained Oswald data as JSON attachments. |
| `/privacy forget-memory <id>` | Stop using a memory immediately and schedule its retained content for scrubbing. |
| `/privacy delete-memory <id>` | Immediately delete one memory and its linked source material. |
| `/privacy delete-candidate <id>` | Delete one memory candidate and any memory published from it. |
| `/privacy delete-session` | Delete the current conversation generation. |
| `/privacy delete-all-memories` | Request deletion of all memories and candidates while keeping your account. |
| `/privacy delete-account` | Request deletion of your account and retained Oswald data. |
| `/privacy confirm <code>` | Confirm a pending bulk deletion with its one-time code. |

Bulk memory deletion and account deletion require confirmation. Confirmation codes expire after 10 minutes.

### Backup And Restore

Oswald's canonical state is `data/database/oswald.db`. SQLite runs in WAL mode, so do not copy only the `.db` file while Oswald is running. Use SQLite's online backup command against the live database, or stop Oswald cleanly before copying the database together with any `-wal` and `-shm` files:

```bash
sqlite3 data/database/oswald.db ".backup '/secure-backups/oswald.db'"
```

Keep the exact `MCP_CONFIG_ENCRYPTION_KEY` with the backup in a separate secrets store. MCP URLs and headers in a restored database cannot be decrypted with a different key. Protect backups as sensitive user data and apply retention and deletion policies to backups and log sinks independently; application privacy commands cannot erase copies already retained outside the live database.

For restore, stop Oswald, replace the database with the backup, remove stale destination `-wal` and `-shm` files, preserve restrictive filesystem permissions, and verify it before startup:

```bash
sqlite3 data/database/oswald.db "PRAGMA integrity_check; PRAGMA foreign_key_check;"
```

`integrity_check` must return `ok`, and `foreign_key_check` must return no rows. Restore into a compatible Oswald release and retain the original backup until startup and these checks succeed.

### Admin Commands

| Command | Usage | Description |
| --- | --- | --- |
| `/users` | `/users` | List canonical users. |
| `/user` | `/user <canonical_id>` | Show one user's account, admin, and ban details. |
| `/admin` | `/admin <canonical_id>` | Grant administrator access to a user. |
| `/unadmin` | `/unadmin <canonical_id>` | Remove administrator access from a user. |
| `/ban` | `/ban <canonical_id> [reason]` | Ban a user from using Oswald. |
| `/unban` | `/unban <canonical_id>` | Unban a user. |
| `/deleteuser` | `/deleteuser <canonical_id>` | Immediately delete another user and their retained Oswald data. |
| `/global-memory add` | `/global-memory add <memory text>` | Add an administrator-curated fact shared with authenticated users; exact normalized duplicates are rejected. |
| `/global-memory list` | `/global-memory list [page]` | List global memories and their stable IDs. |
| `/global-memory forget` | `/global-memory forget <id>` | Permanently delete one global memory. |
| `/mcp global servers` | `/mcp global servers` | List MCP servers visible to all users. |
| `/mcp global add` | `/mcp global add <name> <https-url> [auth-bearer=<token>] [header:<name>=<value>]` | Add or update a global MCP server. |
| `/mcp global remove` | `/mcp global remove <name>` | Remove a global MCP server. |
| `/mcp global enable` | `/mcp global enable <name>` | Enable a global MCP server. |
| `/mcp global disable` | `/mcp global disable <name>` | Disable a global MCP server. |
| `/mcp global test` | `/mcp global test <name>` | Connect to a global MCP server and report its tool count. |

## Roadmap

- [x] Uncensored tool calling model
- [x] Multi-gateway response routing and queuing
- [x] Persistent conversation history (multi-user context)
- [ ] Support for images, gifs, and files
  - [x] Images
  - [x] GIFs
  - [ ] Files
- [x] Global vs User defined MCP servers
- [ ] Scheduled task (cron tool)
- [ ] STT & TTS support

## License

MIT. See [`LICENSE`](LICENSE).
