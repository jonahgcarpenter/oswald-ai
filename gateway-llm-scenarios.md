# Gateway LLM Scenarios

Use this file to compare what currently happens with what you want to happen for Discord and iMessage gateway routing.

## Shared LLM Payload Shape

Every gateway sends a `broker.Request` to `Agent.Process`. The agent then sends Ollama messages in this order:

| Order | Role | Current behavior |
| --- | --- | --- |
| 1 | `system` | `config/soul.md`, current speaker intro, user `system_rules`, current UTC datetime |
| 2 | history | Retained session turns for `SessionKey`, possibly compacted |
| 3 | `user` | Gateway-built `Prompt` plus current-turn image payloads |

Images are attached only to the current user message. Session memory later stores the text prompt and appends `[Attached N image(s)]` instead of preserving image data.

Relevant code:

- `internal/agent/agent.go:407-473`
- `internal/agent/agent.go:731-743`

## Discord Scenarios

| Scenario | Current: reaches LLM? | Current prompt sent as user content | Current images sent | Current session key | Desired behavior / notes |
| --- | --- | --- | --- | --- | --- |
| Bot-authored message | No | N/A | N/A | N/A |  |
| DM text | Yes | Trimmed message, custom emoji normalized, Discord mentions resolved to `@username` | Accepted image attachments | `discord:dm:<discord_user_id>` |  |
| DM image-only | Yes, if at least one image is accepted | Empty string | Accepted image attachments | `discord:dm:<discord_user_id>` |  |
| DM unsupported attachment-only | Yes | `[User attached an unsupported file: ...]` | None | `discord:dm:<discord_user_id>` |  |
| DM empty or no accepted content | No. Gateway replies `What do you want idiot.` | N/A | N/A | N/A |  |
| Guild message mentioning bot | Yes | Bot mention removed, trimmed, custom emoji normalized, Discord mentions resolved to `@username` | Accepted image attachments | `discord:<channel_id>:<discord_user_id>` |  |
| Guild message not mentioning bot | No, unless it is a reply to bot or account command | N/A | N/A | N/A |  |
| Guild reply to bot | Yes, even without mention | Bot mention removed if present, then reply context is prepended | Current message images only | `discord:<channel_id>:<discord_user_id>` |  |
| Guild `/connect` or `/disconnect` | No. Gateway command handler responds directly | N/A | N/A | N/A |  |
| Reply to any message with content | Yes, if message otherwise qualifies | `[Replying to <username>: "<referenced content>"]\n<user prompt>` | Current message images only | Depends on DM/guild |  |
| Reply to message with empty or unavailable content | Yes, if message otherwise qualifies | `[Replying to <username>'s message, but it is unavailable]\n<user prompt>` or `[Replying to a message that is unavailable]\n<user prompt>` | Current message images only | Depends on DM/guild |  |
| Too many or invalid attachments | Yes, if prompt or accepted images remain | Unsupported files appended as `[User attached unsupported files: ...]` | Up to 4 accepted images | Depends on DM/guild |  |

### Discord Examples

Guild mention:

```text
Input:
<@BOT_ID> look at this <@123>

LLM user content:
look at this @someUsername

LLM images:
current accepted attachments
```

Reply with referenced text:

```text
Input:
what did you mean?

Referenced message:
Alice: "deploy failed"

LLM user content:
[Replying to Alice: "deploy failed"]
what did you mean?
```

### Discord Code References

- Guild routing gate: `internal/gateway/discord/gateway.go:458-466`
- Bot mention removal: `internal/gateway/discord/gateway.go:467-471`
- Unsupported attachment prompt note: `internal/gateway/discord/gateway.go:474-475`
- Reply context formatting: `internal/gateway/discord/gateway.go:518-543`
- Broker request construction: `internal/gateway/discord/gateway.go:565-576`
- Attachment validation/loading: `internal/gateway/discord/gateway.go:621-667`

### Discord Possible Logic Gaps

| Gap | Why it matters | Desired behavior / notes |
| --- | --- | --- |
| `replyIndex` is populated for outbound bot messages but not used while building inbound prompts | Replies to Oswald only use Discord's `referenced_message` content, not the stored session metadata |  |
| Referenced Discord attachments are not included in reply context | Replying to an image-only Discord message becomes unavailable or text-only |  |
| Discord and iMessage handle reply-target images differently | iMessage can attach reply-target images to the LLM request; Discord currently cannot |  |

## iMessage Scenarios

| Scenario | Current: reaches LLM? | Current prompt sent as user content | Current images sent | Current session key | Desired behavior / notes |
| --- | --- | --- | --- | --- | --- |
| Self-authored BlueBubbles event | No | N/A | N/A | N/A |  |
| Webhook with no text and no attachments | No | N/A | N/A | N/A |  |
| Direct chat text | Yes | Trimmed text | Accepted image attachments | `imessage:dm:<normalized_sender_id>` |  |
| Direct chat image-only | Yes, if at least one image is accepted | Empty string | Accepted image attachments | `imessage:dm:<normalized_sender_id>` |  |
| Direct unsupported attachment-only | Yes | `[User attached an unsupported file: ...]` | None | `imessage:dm:<normalized_sender_id>` |  |
| Direct empty after filtering | No. Gateway replies `What do you want idiot.` | N/A | N/A | N/A |  |
| Group chat with `@Oswald` | Yes | `@Oswald` removed after reply context handling | Accepted image attachments | `imessage:<chat_guid>:<normalized_sender_id>` |  |
| Group chat without mention | No, unless it is reply-to-bot or account command | N/A | N/A | N/A |  |
| Group reply to bot | Yes, if reply target resolves as bot-authored | Reply context prepended; mention not required | Current images plus reply-target images if capacity remains | `imessage:<chat_guid>:<normalized_sender_id>` |  |
| Group reply to non-bot without mention | No | N/A | N/A | N/A |  |
| `/connect` or `/disconnect` | No. Gateway command handler responds directly | N/A | N/A | N/A |  |
| Reply target found with text | Yes, if message otherwise qualifies | `[Replying to <name>: "<reply text>"]\n<prompt>` | Current images | Depends on direct/group |  |
| Reply target found with text and images | Yes, if message otherwise qualifies | `[Replying to <name>: "<reply text>" with image attachment]\n<prompt>` or `with N image attachments` | Current images plus loaded reply images, capped at 4 total | Depends on direct/group |  |
| Reply target image-only | Yes, if message otherwise qualifies | `[Replying to <name>'s image attachment]\n<prompt>` or `N image attachments` | Current images plus loaded reply images, capped at 4 total | Depends on direct/group |  |
| Reply target unavailable | Direct: yes. Group: only yes if mention/account command; otherwise ignored because `isReplyToBot` is false | `[Replying to a message that is unavailable]\n<prompt>` if it reaches LLM | Current images | Depends on direct/group |  |
| Too many or invalid attachments | Yes, if prompt or accepted images remain | Unsupported files appended as `[User attached unsupported files: ...]` | Up to 4 accepted images across current and reply-target images | Depends on direct/group |  |

### iMessage Examples

Direct chat with image:

```text
Input:
can you read this?
+ accepted image

LLM user content:
can you read this?

LLM images:
1 normalized image
```

Group mention:

```text
Input:
@Oswald summarize this

LLM user content:
summarize this
```

Reply to bot:

```text
Input:
why?

Reply target:
Oswald: "That failed because auth expired."

LLM user content:
[Replying to Oswald: "That failed because auth expired."]
why?
```

Reply to image:

```text
Input:
what is this?

Reply target:
Alice sent 1 image

LLM user content:
[Replying to Alice's image attachment]
what is this?

LLM images:
reply target image, plus any current message images, capped at 4 total
```

### iMessage Code References

- Current message attachment loading: `internal/gateway/imessage/gateway.go:108-119`
- Sender normalization and contact display name: `internal/gateway/imessage/gateway.go:124-140`
- Reply context lookup/application: `internal/gateway/imessage/gateway.go:144-190`
- Group routing gate: `internal/gateway/imessage/gateway.go:192-203`
- Group mention removal: `internal/gateway/imessage/gateway.go:202-204`
- Broker request construction: `internal/gateway/imessage/gateway.go:239-250`
- Reply lookup helpers: `internal/gateway/imessage/gateway.go:338-499`
- Attachment validation/loading: `internal/gateway/imessage/gateway.go:531-577`
- Message/reply cache: `internal/gateway/imessage/gateway.go:782-843`

### iMessage Possible Logic Gaps

| Gap | Why it matters | Desired behavior / notes |
| --- | --- | --- |
| Group reply-to-bot depends on successfully resolving the reply target | If cache/API lookup misses, a group reply without `@Oswald` is ignored |  |
| `@Oswald` is stripped after synthetic reply context is prepended | Usually fine, but mention matching/removal operates on a prompt that may include generated context text |  |
| Reply-target images are attached to iMessage LLM requests | This is context-rich, but differs from Discord and may surprise the model if not desired |  |

## Attachment Rules

| Rule | Current behavior |
| --- | --- |
| Max images | 4 per request |
| Max image size | 10 MiB per image |
| Accepted source types | JPEG, PNG, WebP, HEIC, HEIF, HEIC sequence, HEIF sequence |
| Normalized output to Ollama | JPEG or PNG |
| Unsupported files | Added to prompt as `[User attached an unsupported file: ...]` or `[User attached unsupported files: ...]` |

Relevant code:

- `internal/media/images.go`
- `internal/media/unsupported.go`

## Questions To Decide

| Question | Decision / notes |
| --- | --- |
| Should Discord and iMessage have identical reply behavior? |  |
| Should replying to Oswald always preserve/reuse the original session key? |  |
| Should replies to non-bot messages in group chats reach the LLM when no mention is present? |  |
| Should reply-target images be sent to the LLM? |  |
| Should unsupported attachment-only messages reach the LLM or receive a gateway response? |  |
| Should image-only messages with empty text be allowed in all contexts? |  |
| Should account commands be allowed without mentioning Oswald in group chats/guilds? |  |
| Should the empty prompt response text be changed? |  |
