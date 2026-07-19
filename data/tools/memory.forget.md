# memory.forget

## Description

Deactivate one exact stored memory belonging to the authenticated current user. It is removed from profiles and retrieval immediately; canonical content is retained only until the configured forgetting grace period expires (30 days by default), then maintenance scrubs it. Call this only when the current user explicitly asks to forget or remove that memory. Use memory.list first when its stable memory ID is not already known. Use the `/privacy delete-memory <id>` gateway command when the user explicitly requires immediate irreversible deletion.

The server verifies the authenticated user, current-turn first-party deletion intent, and tenant ownership. Do not infer intent from quoted, hypothetical, or third-party requests. There is no bulk-delete option; each memory requires its exact ID. Do not use this for ordinary conversation cleanup, public facts, or Oswald's own identity/personality/directives.

## Parameters

| Name      | Type    | Required | Description                                                                                                      |
| --------- | ------- | -------- | ---------------------------------------------------------------------------------------------------------------- |
| memory_id | integer | yes      | Stable positive canonical memory ID returned by memory.list. Immediately deactivates only that exact current-user memory; configured grace-period scrubbing follows. |
