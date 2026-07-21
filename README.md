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
- Global memory stores evidence-backed facts Oswald learns about its own implementation, version, architecture, and capabilities from globally configured MCP tools. These facts are shared with every tenant but cannot override policy or authorization.
- Personal memory stores private details such as your preferences, projects, relationships, and environment. Relevant memories are recalled automatically and follow you across linked accounts.
- Conversation memory preserves recent exchanges and summarizes longer conversations. Oswald can search earlier details from the current conversation when needed.

Oswald automatically extracts useful direct facts from exact first-person clauses, including multiple facts embedded in a longer message, and also forms cautious hypotheses from indirect signals. You can still explicitly request a correction or save. Every memory retains confidence, provenance, evidence, and sensitivity metadata. Relevant low-confidence inferences are presented to the model as uncertain possibilities; later independent or direct evidence reinforces the same claim instead of creating disconnected facts. Memory formation never asks for conversational approval, and inferred memories do not enter the always-present user profile until direct evidence supports them.

After any user triggers a successful tool on a globally configured MCP server, Oswald may propose an exact evidence-backed global fact. User-owned MCP results and user prompt text cannot become global evidence. Global MCP servers are therefore part of the global-memory trust boundary and should be configured only when their results are suitable for shared memory.
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
