# Monitoring

Import `grafana/dashboards/oswald-memory-health.json` into Grafana and select the Loki datasource. The dashboard queries Oswald's structured `service`, `component`, and `event` labels, then parses only aggregate health fields from log JSON.

The dashboard intentionally excludes user, session, request, chat, challenge, operation, memory, candidate, turn, job, and tool identifiers, plus all prompt, response, memory, and error content. Invalidation and deletion lag panels are explicitly labeled as retry/throughput proxies because the current stable events do not expose queue age or due-age metrics.
