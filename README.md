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
- List or forget your stored personal memories

## Memory

Oswald uses four memory layers:

- **Soul:** Operator-managed personality and policy loaded from `data/memory/soul/soul.md`.
- **Global memory:** Administrator-curated facts about Oswald that the model searches when relevant.
- **Personal memory:** Private preferences, projects, relationships, and other durable facts shared across your linked accounts.
- **Conversation memory:** Recent exchanges and summaries that preserve continuity within the current conversation.

Personal memories are formed in the background after a response is delivered. Oswald prioritizes clear first-person facts, tracks confidence and provenance, and labels uncertain inferences accordingly.

Use `/reset` to start a new conversation without deleting personal memory. Use `/memories` to list or forget personal memories. Administrators manage shared facts with the `/global-memory` commands.

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

The WebSocket gateway supports command-line clients, [Home Assistant](https://github.com/jonahgcarpenter/has-oswald-conversation), and other service integrations. It accepts plain-text prompts or JSON messages containing text and images.

First, request a device code:

```bash
curl -sS http://127.0.0.1:8000/auth/device \
  -H 'Content-Type: application/json' \
  -d '{"client_name":"Laptop"}'
```

Approve the returned code from an authenticated conversation:

```bash
/client approve ABCD-EFGH
```

Then poll the token endpoint no faster than the interval returned with the device code:

```bash
curl -sS http://127.0.0.1:8000/auth/token \
  -H 'Content-Type: application/json' \
  -d '{"grant_type":"device_code","device_code":"<device_code>"}'
```

Connect using the returned access token:

```bash
websocat \
  -H="Authorization: Bearer <access_token>" \
  ws://127.0.0.1:8000/ws
```

Access tokens last 15 minutes. Store the refresh token securely and exchange it at `/auth/token` using `grant_type: "refresh_token"` before the access token expires. Each refresh returns a replacement refresh token.

After connecting, send a plain-text prompt or a JSON message. Oswald streams typed events followed by a final response:

```json
{"type":"content","text":"Bitcoin is currently..."}
{"model":"<gateway-route-or-model>","response":"..."}
```

Revoke a client with `/client revoke <client_id>` or `POST /auth/revoke` using its refresh token.

## Bootstrap

When starting with an empty database, Oswald creates a temporary administrator and prints a 15-minute WebSocket access token to the terminal.

1. Connect to the WebSocket gateway using the printed token.
2. Request a device code for the permanent administrator.
3. From the temporary administrator connection, run:
   `/bootstrap admin <user_code> <display_name>`
4. Connect the newly approved permanent administrator.
5. Delete the temporary user with:
   `/deleteuser <canonical_id>`
   The temporary bootstrap client is revoked after the permanent administrator connects.

## Commands

Commands are gateway-level slash commands. They are handled before requests reach the model.

### User Commands

| Command        | Usage                                                                                                               | Description                                                                     |
| -------------- | ------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------- |
| `/help`        | `/help [command]`                                                                                                   | List available commands or show usage for one command.                          |
| `/connect`     | `/connect [code\|cancel]`                                                                                           | Create, confirm, or cancel a 10-minute account-link code.                       |
| `/disconnect`  | `/disconnect [account_number]`                                                                                      | List or disconnect linked accounts. The final account cannot be removed.        |
| `/reset`       | `/reset`                                                                                                            | Clear the current conversation history and load the latest user profile.        |
| `/memories`    | `/memories list`, `/memories forget <id\|all>`                                                                      | List or forget your personal memories.                                          |
| `/client`      | `/client approve <code>`, `/client approve-new <code> <display_name>`, `/client list`, `/client revoke <client_id>` | Approve and manage WebSocket clients.                                           |
| `/mcp servers` | `/mcp servers`                                                                                                      | List your user-scoped MCP servers.                                              |
| `/mcp add`     | `/mcp add <name> <https-url> [auth-bearer=<token>] [header:<name>=<value>]`                                         | Add or update a user-scoped MCP server. URLs and headers are encrypted at rest. |
| `/mcp remove`  | `/mcp remove <name>`                                                                                                | Remove one of your MCP servers.                                                 |
| `/mcp enable`  | `/mcp enable <name>`                                                                                                | Enable one of your MCP servers.                                                 |
| `/mcp disable` | `/mcp disable <name>`                                                                                               | Disable one of your MCP servers.                                                |
| `/mcp test`    | `/mcp test <name>`                                                                                                  | Connect to one of your MCP servers and report its tool count.                   |

`/connect`, `/disconnect`, `/client`, and every `/memories` operation require an authenticated identity and can be used in private or group conversations. `/bootstrap` additionally requires the temporary WebSocket bootstrap client. In Discord servers and iMessage groups, slash commands must mention Oswald. Account-link codes, memory lists, client details, and MCP credentials may be visible to other group members, so use sensitive commands in a private conversation when appropriate.

### Memory Commands

| Command                 | Description                                                                                                                |
| ----------------------- | -------------------------------------------------------------------------------------------------------------------------- |
| `/memories list`        | Attach a complete text file containing every active memory's stable ID, category, and statement.                           |
| `/memories forget <id>` | Permanently hard-delete one durable memory and its memory-only artifacts. Conversation transcripts remain unchanged.       |
| `/memories forget all`  | Permanently delete all stored user information, reset every session, and preserve only account and authentication state.   |

### Admin Commands

| Command                 | Usage                                                                              | Description                                                                                                  |
| ----------------------- | ---------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------ |
| `/users`                | `/users`                                                                           | List canonical users.                                                                                        |
| `/user`                 | `/user <canonical_id>`                                                             | Show one user's account, admin, and ban details.                                                             |
| `/admin`                | `/admin <canonical_id>`                                                            | Grant administrator access to a user.                                                                        |
| `/unadmin`              | `/unadmin <canonical_id>`                                                          | Remove administrator access from a user.                                                                     |
| `/ban`                  | `/ban <canonical_id> [reason]`                                                     | Ban a user from using Oswald.                                                                                |
| `/unban`                | `/unban <canonical_id>`                                                            | Unban a user.                                                                                                |
| `/deleteuser`           | `/deleteuser <canonical_id>`                                                       | Immediately delete another user and their retained Oswald data.                                              |
| `/global-memory add`    | `/global-memory add <memory text>`                                                 | Add an administrator-curated fact shared with authenticated users; exact normalized duplicates are rejected. |
| `/global-memory list`   | `/global-memory list [page]`                                                       | List global memories and their stable IDs.                                                                   |
| `/global-memory forget` | `/global-memory forget <id>`                                                       | Permanently delete one global memory.                                                                        |
| `/mcp global servers`   | `/mcp global servers`                                                              | List MCP servers visible to all users.                                                                       |
| `/mcp global add`       | `/mcp global add <name> <https-url> [auth-bearer=<token>] [header:<name>=<value>]` | Add or update a global MCP server.                                                                           |
| `/mcp global remove`    | `/mcp global remove <name>`                                                        | Remove a global MCP server.                                                                                  |
| `/mcp global enable`    | `/mcp global enable <name>`                                                        | Enable a global MCP server.                                                                                  |
| `/mcp global disable`   | `/mcp global disable <name>`                                                       | Disable a global MCP server.                                                                                 |
| `/mcp global test`      | `/mcp global test <name>`                                                          | Connect to a global MCP server and report its tool count.                                                    |

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
