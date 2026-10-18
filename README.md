# Oswald AI - Uncensored Digital Servant

> Fully local, fully uncensored, with no paid API required.

## Overview

Oswald AI is a local-first, self-hosted assistant that brings your chosen language model to iMessage, Discord, and a OpenAI-compatible API.
It combines tools, private profile memory, conversation continuity, image understanding, and connected services while keeping you in control of your data.

## Features

- Chat through iMessage, Discord, or the OpenAI-compatible API
- Send text, images, animated GIFs, and replies with quoted context
- Search the web, generate and edit images, and use connected MCP tools
- Remember your preferences, projects, and other useful details across conversations
- Keep continuity in long conversations and search earlier conversation details

## Usage

### Discord/iMessage Bot

In DMs or direct chats, send any message:

```text
What is the current weather?
```

In server channels or group chats, start your message with a mention of Oswald:

```text
@Oswald What is the capital of France?
```

You can also reply to any message and mention Oswald to include that message as context:

```text
[Replying to Jonah: "The capital of the US is New York"]
@Oswald Is this true?
```

Recognized direct replies to Oswald can continue the conversation without another mention:

```text
[Replying to Oswald: "The Cincinnati Bengals are easily the best team in the NFL"]
Can you elaborate on that?
```

### API

Oswald also serves an OpenAI-compatible API on loopback by default (`127.0.0.1:8000`,
configurable via `api_host`/`api_port`), authenticated per profile with the bearer key in
`OPENAI_API_KEY`:

```bash
curl http://127.0.0.1:8000/v1/chat/completions \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model": "oswald", "messages": [{"role": "user", "content": "Hello Oswald"}]}'
```

Conversations over the API are stateless — nothing is stored — and responses are text-only:
no attachments and no slash commands.

To use Oswald as a Home Assistant voice assistant, see
[has-oswald-conversation](https://github.com/jonahgcarpenter/has-oswald-conversation)

## Chat Commands

Chat commands are gateway-level slash commands. They are handled before requests reach the model.

In Discord servers and iMessage groups, slash commands must start with a mention of Oswald.

| Command    | Usage             | Description                                                                                            |
| ---------- | ----------------- | ------------------------------------------------------------------------------------------------------ |
| `/help`    | `/help [command]` | List available commands or show usage for one command.                                                 |
| `/new`     | `/new`            | Start a fresh session with the latest memory files; past conversations remain searchable until expiry. |
| `/stop`    | `/stop`           | Stop the currently running response in this conversation without removing queued prompts.              |
| `/approve` | `/approve`        | Approve the pending terminal command awaiting review.                                                  |
| `/deny`    | `/deny`           | Deny the pending terminal command awaiting review.                                                     |

## CLI Commands

Operator commands run from a terminal against your `.oswald/` root:

| Command                        | Description                                                                              |
| ------------------------------ | ---------------------------------------------------------------------------------------- |
| `oswald setup`                 | Seed missing `config.yaml`/`.env`/`SOUL.md` entries for first install; never overwrites. |
| `oswald start`                 | Run the Oswald service in the foreground.                                                |
| `oswald profile create <name>` | Provision a new named profile subtree.                                                   |
| `oswald profile delete <name>` | Remove a named profile subtree (refuses `default`, requires confirmation).               |
| `oswald mcp auth <server>`     | Interactively authorize an MCP server (browser or device flow).                          |

## Documentation

Everything resides in [`AGENTS.md`](AGENTS.md).

## License

MIT. See [`LICENSE`](LICENSE).
