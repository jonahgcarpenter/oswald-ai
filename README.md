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

You can also reply to a message and mention Oswald to include that message as context:

```text
[Replying to Jonah: "The capital of the US is New York"]
@Oswald Is this true?
```

Recognized replies to Oswald can continue the conversation without another mention:

```text
[Reply to Oswald's message]
Can you elaborate on that?
```

## Commands

Commands are gateway-level slash commands. They are handled before requests reach the model.

In Discord servers and iMessage groups, slash commands must start with a mention of Oswald.

| Command | Usage             | Description                                                                                            |
| ------- | ----------------- | ------------------------------------------------------------------------------------------------------ |
| `/help` | `/help [command]` | List available commands or show usage for one command.                                                 |
| `/new`  | `/new`            | Start a fresh session with the latest memory files; past conversations remain searchable until expiry. |
| `/stop` | `/stop`           | Stop the currently running response in this conversation without removing queued prompts.              |

## License

MIT. See [`LICENSE`](LICENSE).
