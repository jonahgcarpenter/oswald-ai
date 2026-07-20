# time.current

## Description

Return the authoritative current date and time in a resolved IANA timezone.

Use this tool whenever an answer depends on the current date, time, weekday, year,
or a conversion of the current time. Resolve the timezone in this order:

1. Use a timezone or location explicitly supplied in the current request.
2. Otherwise call user_memory_search for the user's timezone or location.
3. A remembered location may be mapped to an IANA timezone only when the
   mapping is unambiguous.
4. If the timezone remains unknown or ambiguous, ask the user for their
   timezone or location. Do not call this tool until they answer.

## Parameters

| Name     | Type   | Required | Description                                                                    |
| -------- | ------ | -------- | ------------------------------------------------------------------------------ |
| timezone | string | yes      | IANA timezone such as America/New_York, Europe/London, Asia/Kathmandu, or UTC. |
