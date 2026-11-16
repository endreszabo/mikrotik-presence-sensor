# Configuration reference

Configuration comes from three layers. Later layers win:

1. Built-in defaults (listed below).
2. A YAML file, given with `-config path` or `MPS_CONFIG_FILE=path`. It is
   optional, and unknown keys are rejected.
3. Environment variables prefixed with `MPS_`.

The same struct, parsed by
[caarlos0/env](https://github.com/caarlos0/env), defines both the YAML keys
and the variables, so they always match:

- **Name:** uppercase the YAML path and join it with `_`. For example
  `mqtt.tls.ca_file` → `MPS_MQTT_TLS_CA_FILE`.
- **List items:** take an index. For example `devices[2].snmp.version` →
  `MPS_DEVICES_2_SNMP_VERSION`. A variable for an index that the YAML file
  already defines overrides only that field. Higher indexes add new entries.
- **Lists of values** are comma-separated: `MPS_PEOPLE_0_MACS=aa:..,bb:..`.
- **Durations** use Go syntax: `30s`, `3m`, `1h30m`.
- **Secrets** can be put in files, which suits Docker secrets: every
  `password`/`community` key has a `*_file` sibling.

To validate a configuration and see the result, secrets masked:

```sh
mikrotik-presence-sensor -check-config -config config.yaml
docker compose run --rm mikrotik-presence-sensor -check-config
```

## `log`

| key | env | default | description |
|---|---|---|---|
| `format` | `MPS_LOG_FORMAT` | `zorp` | `zorp` (zorplogger line format) or `json` |
| `level` | `MPS_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`; `debug-4` additionally logs unmatched syslog lines |

## `health`

| key | env | default | description |
|---|---|---|---|
| `listen` | `MPS_HEALTH_LISTEN` | `[::]:8080` | HTTP listener for `/healthz`, `/readyz`, `/status` |
| `require_mqtt` | `MPS_HEALTH_REQUIRE_MQTT` | `true` | `/healthz` returns 503 while the broker is disconnected |
| `metrics` | `MPS_HEALTH_METRICS` | `true` | serve Prometheus metrics on `/metrics` ([reference](metrics.md)) |

The `healthcheck` subcommand reads `health.listen` from the configuration (or
`MPS_HEALTH_LISTEN`) and probes loopback. It tries `::1` first and then
`127.0.0.1`. To override, pass `healthcheck -url http://…/healthz`.

## `mqtt`

| key | env | default | description |
|---|---|---|---|
| `broker` | `MPS_MQTT_BROKER` | `tcp://[::1]:1883` | `tcp://`, `ssl://`/`mqtts://`, `ws://`, `wss://`. Bracket IPv6 literals. |
| `username` | `MPS_MQTT_USERNAME` | | |
| `password` | `MPS_MQTT_PASSWORD` | | |
| `password_file` | `MPS_MQTT_PASSWORD_FILE` | | read the password from this file |
| `client_id` | `MPS_MQTT_CLIENT_ID` | `mikrotik-presence-<node_id>` | |
| `base_topic` | `MPS_MQTT_BASE_TOPIC` | `mikrotik-presence` | state topics live under `<base_topic>/<node_id>/` |
| `discovery_prefix` | `MPS_MQTT_DISCOVERY_PREFIX` | `homeassistant` | must match HA's MQTT discovery prefix |
| `node_id` | `MPS_MQTT_NODE_ID` | `mikrotik_presence` | `[a-z0-9_-]`; use different IDs for several instances |
| `keepalive` | `MPS_MQTT_KEEPALIVE` | `30s` | |
| `connect_timeout` | `MPS_MQTT_CONNECT_TIMEOUT` | `10s` | |
| `max_reconnect_interval` | `MPS_MQTT_MAX_RECONNECT_INTERVAL` | `1m` | reconnect backoff cap; retries continue forever |
| `tls.ca_file` | `MPS_MQTT_TLS_CA_FILE` | | CA bundle used to verify the broker |
| `tls.cert_file` / `tls.key_file` | `MPS_MQTT_TLS_CERT_FILE` / `MPS_MQTT_TLS_KEY_FILE` | | client certificate authentication |
| `tls.server_name` | `MPS_MQTT_TLS_SERVER_NAME` | | expected certificate name, useful when the broker is given as an IP |
| `tls.insecure_skip_verify` | `MPS_MQTT_TLS_INSECURE_SKIP_VERIFY` | `false` | testing only |

## `presence`

| key | env | default | description |
|---|---|---|---|
| `consider_home` | `MPS_PRESENCE_CONSIDER_HOME` | `3m` | grace period after the last sighting before `not_home`; also the startup grace |
| `event_ttl` | `MPS_PRESENCE_EVENT_TTL` | = `consider_home` | how long a `seen` event, or a DHCP lease's `last-seen`, counts as a sighting |
| `sources` | `MPS_PRESENCE_SOURCES` | all kinds | which kinds count toward presence; kinds not listed are not polled either |
| `arp_states` | `MPS_PRESENCE_ARP_STATES` | `reachable,delay,probe,dynamic` | ARP entry states that count (`dynamic` is the state SNMP's legacy ARP table reports) |
| `neighbor_states` | `MPS_PRESENCE_NEIGHBOR_STATES` | `reachable,delay,probe` | IPv6 neighbor states that count |
| `stale_after_failed_polls` | `MPS_PRESENCE_STALE_AFTER_FAILED_POLLS` | `3` | consecutive failed polls of a table after which its data is dropped |

### Kinds

| kind | REST path | SNMP table | notes |
|---|---|---|---|
| `wifi` | `/interface/wifi/registration-table` | MIKROTIK-MIB `mtxrWlRtabTable` (best effort) | RouterOS 7 `wifi` package, including clients of CAPsMAN-managed APs on the controller. **REST recommended**: the public MIKROTIK-MIB has no separate table for this package. Never auto-probed over SNMP. |
| `wireless` | `/interface/wireless/registration-table` | MIKROTIK-MIB `mtxrWlRtabTable` | legacy `wireless` package; WDS/AP peers are ignored |
| `capsman` | `/caps-man/registration-table` | MIKROTIK-MIB `mtxrWlCMRtabTable` | legacy CAPsMAN controller |
| `bridge_host` | `/interface/bridge/host` | Q-BRIDGE-MIB `dot1qTpFdbTable`, falling back to BRIDGE-MIB `dot1dTpFdbTable` | learned entries only; local entries are ignored |
| `neighbor` | `/ipv6/neighbor` | IP-MIB `ipNetToPhysicalTable` (IPv6 rows) | |
| `arp` | `/ip/arp` | IP-MIB `ipNetToPhysicalTable` (IPv4 rows), falling back to `ipNetToMediaTable` | |
| `dhcp` | `/ip/dhcp-server/lease` | n/a | only `bound` leases with `last-seen` younger than `event_ttl` count |

## `defaults` and `devices`

`defaults` holds device settings. Each device inherits every field it leaves
empty. In env form, defaults are `MPS_DEFAULTS_…` and devices are
`MPS_DEVICES_<n>_…`.

| key | env suffix | default | description |
|---|---|---|---|
| `name` | `NAME` | (required) | unique device name; shown in attributes and HA diagnostics |
| `host` | `HOST` | (required) | IPv6 or IPv4 address, or DNS name. No brackets, no URL. |
| `syslog_sources` | `SYSLOG_SOURCES` | | other addresses this device sends logs from (for example its IPv4 address) |
| `transport` | `TRANSPORT` | `rest` | `rest` or `snmp` |
| `kinds` | `KINDS` | `auto` | `auto` probes the device and stops polling tables it lacks. You can instead give an explicit list of kinds, or `none` for log-only devices. |
| `poll_interval` | `POLL_INTERVAL` | `30s` | log events trigger extra polls, debounced to 2s |
| `timeout` | `TIMEOUT` | `5s` | per request |
| `rest.port` | `REST_PORT` | `443` | the `www-ssl` service port |
| `rest.username` | `REST_USERNAME` | | required for REST |
| `rest.password` / `rest.password_file` | `REST_PASSWORD` / `REST_PASSWORD_FILE` | | |
| `rest.ca_file` | `REST_CA_FILE` | system roots | CA that signed the device certificate |
| `rest.server_name` | `REST_SERVER_NAME` | | expected certificate name when `host` is an IP not in the certificate |
| `rest.insecure_skip_verify` | `REST_INSECURE_SKIP_VERIFY` | `false` | accept any certificate (testing only; logs a warning) |
| `snmp.port` | `SNMP_PORT` | `161` | |
| `snmp.version` | `SNMP_VERSION` | `2c` | `2c` or `3` |
| `snmp.community` / `snmp.community_file` | `SNMP_COMMUNITY` / `SNMP_COMMUNITY_FILE` | `public` | v2c only |
| `snmp.retries` | `SNMP_RETRIES` | `1` | |
| `snmp.max_repetitions` | `SNMP_MAX_REPETITIONS` | `25` | GETBULK size |
| `snmp.security_level` | `SNMP_SECURITY_LEVEL` | `authPriv` | `noAuthNoPriv`, `authNoPriv`, `authPriv` |
| `snmp.username` | `SNMP_USERNAME` | | v3 user (the RouterOS community name) |
| `snmp.auth_protocol` | `SNMP_AUTH_PROTOCOL` | `SHA256` | `MD5`, `SHA`, `SHA224`, `SHA256`, `SHA384`, `SHA512` |
| `snmp.auth_password` / `_file` | `SNMP_AUTH_PASSWORD` / `_FILE` | | |
| `snmp.priv_protocol` | `SNMP_PRIV_PROTOCOL` | `AES` | `DES`, `AES`, `AES192`, `AES256`, `AES192C`, `AES256C` |
| `snmp.priv_password` / `_file` | `SNMP_PRIV_PASSWORD` / `_FILE` | | |
| `snmp.context_name` | `SNMP_CONTEXT_NAME` | | |

## `syslog`

| key | env | default | description |
|---|---|---|---|
| `enabled` | `MPS_SYSLOG_ENABLED` | `true` | |
| `udp_listen` | `MPS_SYSLOG_UDP_LISTEN` | `[::]:5514` | empty disables UDP |
| `tcp_listen` | `MPS_SYSLOG_TCP_LISTEN` | `[::]:5514` | empty disables TCP. Supports newline and RFC 6587 octet-counted framing. |
| `allowed_sources` | `MPS_SYSLOG_ALLOWED_SOURCES` | | extra addresses or CIDRs allowed to send. Configured devices are always allowed. `any` disables the check. |
| `disable_default_rules` | `MPS_SYSLOG_DISABLE_DEFAULT_RULES` | `false` | |
| `rules[]` | `MPS_SYSLOG_RULES_<n>_…` | | custom rules, evaluated before the built-in ones |

Senders are matched to devices by source address. Device host names are
resolved again every 5 minutes. Messages from unknown senders are dropped,
with a rate-limited warning, and counted in `/status`.

### Rules

| field | description |
|---|---|
| `name` | for logs |
| `pattern` | Go regular expression, matched anywhere in the message after the syslog header. `{mac}` expands to a MAC address capture group. Optional groups: `iface`, `ip`, `event`. |
| `event` | `join` (present until a `leave`, or until a later poll of that table no longer shows it), `leave`, or `seen` (present for `event_ttl`). If empty, the `event` group decides: connected/associated/join/up → join; disconnected/deassigned/leave/down → leave; assigned/seen → seen. |
| `kind` | kind to attribute the sighting to. Leave empty for a generic "log" sighting. |

The first matching rule wins. These are the built-in rules:

```yaml
- {name: wireless, kind: wireless, pattern: '{mac}@(?P<iface>[^\s:@]+): (?P<event>connected|disconnected)\b'}
- {name: wifi,     kind: wifi,     pattern: '{mac}@(?P<iface>[^\s:@]+),? (?P<event>connected|disconnected)\b'}
- {name: dhcp-deassigned, kind: dhcp, event: leave, pattern: '\bdeassigned (?P<ip>\S+) (?:for|from) {mac}'}
- {name: dhcp-assigned,   kind: dhcp, event: seen,  pattern: '\bassigned (?P<ip>\S+) (?:for|to) {mac}'}
```

They match lines such as:

```
AA:BB:CC:DD:EE:FF@wlan1: connected, signal strength -62
AA:BB:CC:DD:EE:FF@wifi2 disconnected, connection lost, signal strength -82
dhcp1 assigned 192.0.2.10 for AA:BB:CC:DD:EE:FF Pixel-8
```

RouterOS does not log ARP, neighbor, or FDB changes. To get those in real time
anyway, have a script `:log` a line and add a matching custom rule.
`examples/routeros/05-dhcp-lease-script.rsc` shows the pattern.

## `people`

| key | env | description |
|---|---|---|
| `id` | `MPS_PEOPLE_<n>_ID` | `[a-z0-9_-]`; used in entity IDs and topics |
| `name` | `MPS_PEOPLE_<n>_NAME` | display name (defaults to `id`) |
| `macs` | `MPS_PEOPLE_<n>_MACS` | one or more MACs in any common notation; a MAC can belong to only one person |
| `icon` | `MPS_PEOPLE_<n>_ICON` | MDI icon for the tracker, e.g. `mdi:account` |
