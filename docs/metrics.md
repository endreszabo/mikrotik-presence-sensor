# Prometheus metrics

Metrics are served at `GET /metrics` on the health listener (`health.listen`,
default `[::]:8080`). To turn them off, set `health.metrics: false`
(`MPS_HEALTH_METRICS=false`). All metric names start with
`mikrotik_presence_`. The standard `go_*` and `process_*` collectors are
included as well.

```yaml
scrape_configs:
  - job_name: mikrotik-presence
    static_configs:
      - targets: ["[2001:db8:0:10::5]:8080"]
```

[`examples/prometheus/`](../examples/prometheus) has this scrape config and a
set of alert rules, validated with `promtool`.

## Device availability

| metric | type | labels | description |
|---|---|---|---|
| `device_up` | gauge | `device`, `transport` | 1 if the last poll succeeded for every table, 0 otherwise |
| `device_last_success_timestamp_seconds` | gauge | `device` | Unix time of the last fully successful poll |
| `device_consecutive_failures` | gauge | `device` | failed polls in a row (data is dropped at `presence.stale_after_failed_polls`) |
| `polls_total` | counter | `device`, `result` | poll cycles by `success` or `error` |
| `syslog_last_message_timestamp_seconds` | gauge | `device` | last log message received from the device; the liveness signal for `kinds: [none]` devices |

Device series appear after the first poll. A device that has never been
polled has no `device_up` series yet.

## Polling latency

| metric | type | labels | description |
|---|---|---|---|
| `poll_duration_seconds` | histogram | `device` | whole poll cycle; for SNMP this includes the session setup |
| `poll_kind_duration_seconds` | histogram | `device`, `kind` | time to fetch one table (REST request or SNMP walk) |
| `poll_kind_errors_total` | counter | `device`, `kind` | failed table fetches |

Buckets run from 5 ms to 30 s. Tables that a device lacks (pruned in
`kinds: auto` mode) are not recorded.

## Discovered items

| metric | type | labels | description |
|---|---|---|---|
| `discovered_items` | gauge | `device`, `kind` | entries in the table at the last successful fetch: all clients/hosts, not just tracked people |

This tells you how many Wi-Fi clients each AP has, how big the ARP and
neighbor caches are, and how many hosts each switch has learned. After a
failed fetch the gauge keeps its last value; use `poll_kind_errors_total` or
`device_up` to spot failures.

## Syslog

| metric | type | labels | description |
|---|---|---|---|
| `syslog_messages_total` | counter | `device`, `result` | messages from allowed senders, `matched` by a rule or `ignored` |
| `syslog_events_total` | counter | `device`, `kind`, `event` | presence events (`join`, `leave`, `seen`) per kind (`log` for custom rules without a kind); counts all MACs, tracked or not |
| `syslog_dropped_total` | counter | | messages from unauthorized senders |

The `device` label is the configured device name. For senders admitted
through `syslog.allowed_sources`, it is the sender's address.

## Presence and MQTT

| metric | type | labels | description |
|---|---|---|---|
| `person_home` | gauge | `person` | 1 home, 0 not home; absent while unknown after startup |
| `people_home` | gauge | | people currently home |
| `mqtt_connected` | gauge | | 1 while the broker connection is up |
| `build_info` | gauge | `version`, `goversion` | always 1 |

## Useful queries

```promql
# devices currently failing
mikrotik_presence_device_up == 0

# seconds since each device was last polled successfully
time() - mikrotik_presence_device_last_success_timestamp_seconds

# p95 latency per device and table
histogram_quantile(0.95, sum by (device, kind, le) (rate(mikrotik_presence_poll_kind_duration_seconds_bucket[5m])))

# Wi-Fi clients per AP
sum by (device) (mikrotik_presence_discovered_items{kind=~"wifi|wireless|capsman"})

# syslog event rate per device and event type (events/minute)
sum by (device, event) (rate(mikrotik_presence_syslog_events_total[5m])) * 60

# roaming / reconnect churn: leave events per hour per AP
sum by (device) (increase(mikrotik_presence_syslog_events_total{event="leave"}[1h]))
```
