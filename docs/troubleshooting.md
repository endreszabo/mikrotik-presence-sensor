# Troubleshooting

Start with `/status`. It shows each person's current observations and state,
each device's last poll result and error, and the syslog counters:

```sh
curl -s 'http://[::1]:8080/status' | jq
```

For more detail, set `log.level: debug` (`MPS_LOG_LEVEL=debug`). Debug
logging records every poll's entry counts and every matched syslog line.

## A person is never detected

- **Randomized MAC.** The phone uses a private address on your Wi-Fi. The
  sensor logs a warning at startup for locally administered MACs in the
  config. Compare against the registration table on the AP:
  `/interface/wifi/registration-table print`.
- **Kind not polled.** Check `devices[].kinds` in `/status`. In `auto` mode,
  tables the device lacks are dropped after the first poll; this is logged
  at info level.
- **ARP/neighbor state filtered.** Entries in `stale` state don't count. Look
  at `observations` in `/status`. If the person has no observations at all
  while the table shows them, check `arp_states` and `neighbor_states`.

## A person stays home after leaving

- An ARP or neighbor entry may stay `reachable`. Remove `arp` or `neighbor`
  from `presence.sources`, or rely on Wi-Fi kinds only.
- A bridge host entry ages out only after the bridge `ageing-time` (5 minutes
  by default) plus `consider_home`.
- A device stopped responding. Its data is dropped only after
  `stale_after_failed_polls` failures. Check the device's polling
  diagnostics entity.

## REST errors

| log / status | cause |
|---|---|
| `authentication failed (HTTP 401)` | wrong user or password, the user's `address` restriction, or a missing `rest-api` policy |
| `x509: certificate signed by unknown authority` | set `rest.ca_file` to the CA that signed the `www-ssl` certificate |
| `x509: certificate is valid for …, not …` | set `rest.server_name` to a name contained in the certificate |
| `connection refused` | `www-ssl` disabled, a different port (`rest.port`), or the service `address` restriction |
| `not supported on this device (HTTP 400 no such command…)` | the package isn't installed; harmless in `auto` mode |

## SNMP errors

- `request timeout`: wrong credentials (SNMPv3 failures often look like
  timeouts), a community `addresses` restriction, or a firewall. Test with
  `snmpbulkwalk` (see [mikrotik-setup.md](mikrotik-setup.md)).
- Empty Wi-Fi tables while REST shows clients: the AP runs the new `wifi`
  package, whose clients aren't exported through `mtxrWlRtabTable` on every
  RouterOS version. Use REST for that AP.

## Syslog events don't arrive

- `/status` → `syslog.dropped_unauthorized` increasing: the sender's address
  isn't the device `host`. Add it to that device's `syslog_sources`, or to
  `syslog.allowed_sources`.
- `received` increases but `matched` doesn't: run with `log.level: debug-4`
  to log every unmatched line after header stripping. Then adjust or add a
  rule (see [configuration.md](configuration.md#rules)).
- Nothing arrives at all:
  - Check the RouterOS logging action (`/system logging action print`) and
    the firewall on the sensor host.
  - With Docker bridge networking, IPv6 syslog arrives from the gateway
    address unless the userland proxy is disabled. The default compose file
    uses host networking for this reason.
- Send a test event yourself, using util-linux `logger`:
  ```sh
  logger -n ::1 -P 5514 -d 'wireless,info AA:BB:CC:00:00:01@wifi1 connected, signal strength -55'
  ```
  The loopback source must be allowed for this to work:
  `MPS_SYSLOG_ALLOWED_SOURCES=::1`.

## MQTT

- The container is `unhealthy` while the broker is unreachable. This is by
  design (`health.require_mqtt`). The logs show `lost connection to broker,
  reconnecting`, and the sensor recovers on its own.
- Entities don't appear in HA: check the MQTT integration's discovery prefix
  against `mqtt.discovery_prefix`. Check the broker with
  `mosquitto_sub -v -t 'homeassistant/+/mikrotik_presence/#'`.
- Duplicate entities after renaming `mqtt.node_id`: the old node's retained
  discovery topics remain. Clear them with
  `mosquitto_pub -r -n -t <topic>`, or delete the devices in HA.
