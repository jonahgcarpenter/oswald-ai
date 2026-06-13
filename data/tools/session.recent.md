# session.recent

## Description

Read recent exchanges from the current conversation session. Use this when the user sends a vague follow-up, or if the needed context is not present in the current prompt. Do not use it unless prior session context is necessary to answer.

## Parameters

| Name   | Type    | Required | Description                                                                                  |
| ------ | ------- | -------- | -------------------------------------------------------------------------------------------- |
| offset | integer | no       | Which recent exchange to start from: `1` is the newest completed exchange, `2` is one older. |
| count  | integer | no       | Number of exchanges to return, from `1` to `3`. Defaults to `1`.                             |
