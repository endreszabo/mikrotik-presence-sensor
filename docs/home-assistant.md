# Home Assistant

## Prerequisites

- An MQTT broker. Use your existing one (for example the Mosquitto add-on),
  or start the bundled one with `docker compose --profile broker up -d`.
- The **MQTT integration** in Home Assistant, connected to that broker, with
  discovery enabled. Discovery is on by default and uses the prefix
  `homeassistant`.

No custom integration or HACS install is needed. Everything arrives through
MQTT discovery and is retained, so entities survive restarts of HA, the
broker, and the sensor.

## Entities

For each person in `people` (example: `id: alice`, `name: Alice`), you get one
HA device, **"Alice presence"**, with:

| entity | states | purpose |
|---|---|---|
| `device_tracker.alice_presence` | `home` / `not_home` | assign it to the HA *person* |
| `binary_sensor.alice_presence_home` | `on` / `off` (device class *presence*) | handy in conditions and dashboards |

Both entities carry these attributes:

| attribute | example | description |
|---|---|---|
| `mac` | `aa:bb:cc:00:00:01` | the MAC behind the best current sighting |
| `device` | `ap-living-room` | the AP, switch, or router reporting it |
| `interface` | `wifi2` | the interface it was seen on |
| `ssid`, `band`, `signal` | `home`, `5ghz-ax`, `-58` | Wi-Fi sightings only |
| `ip_addresses` | `["2001:db8::1:10", "fe80::…", "192.0.2.10"]` | from neighbor, ARP, and DHCP tables; IPv6 first |
| `sources` | `["arp", "neighbor", "wifi"]` | kinds currently seeing the person |
| `seen_by` | `["ap-living-room", "core-router"]` | devices currently seeing the person |
| `last_seen` | ISO timestamp | most recent sighting |
| `since` | ISO timestamp | time of the last state change |

Attributes are republished when something significant changes: the AP,
interface, IPs, or sources. Signal strength alone doesn't trigger a publish,
which keeps the recorder quiet.

A bridge device, **"MikroTik presence bridge"**, provides:

| entity | description |
|---|---|
| `binary_sensor.…_connectivity` | whether the sensor is online (MQTT LWT) |
| `sensor.…_people_home` | number of people home |
| `sensor.…_devices_polled_ok` | number of devices whose last poll succeeded (diagnostic) |
| `binary_sensor.…_<device>_polling` | per polled device: last poll OK (diagnostic). Attributes: errors, kinds, timestamps. |

While the sensor is offline, all person entities show *unavailable* rather
than a stale state.

## Linking to persons

Go to **Settings → People → Alice → Track device** and pick
`device_tracker.alice_presence`. You can combine it with the companion app's
GPS tracker. HA prefers the stationary (router) tracker while you're home.

## Topics

| topic | payload |
|---|---|
| `mikrotik-presence/<node_id>/availability` | `online` / `offline` (LWT, retained) |
| `mikrotik-presence/<node_id>/person/<id>/state` | `home` / `not_home` (retained) |
| `mikrotik-presence/<node_id>/person/<id>/attributes` | JSON attributes (retained) |
| `mikrotik-presence/<node_id>/bridge/state` | `{"people_home":1,"home":["alice"],"devices_ok":3,"devices_total":3,…}` |
| `mikrotik-presence/<node_id>/device/<device>/state` | `ON` / `OFF` |
| `homeassistant/<component>/<node_id>/<object>/config` | discovery |

The sensor watches `homeassistant/status`. When HA announces `online` after a
restart, the sensor publishes discovery and states again. If you remove a
person or device from the configuration, the next start deletes its retained
discovery entry, and HA removes the entity.

## Automation examples

Turn off the lights when the last person leaves:

```yaml
automation:
  - alias: Everyone left
    triggers:
      - trigger: numeric_state
        entity_id: sensor.mikrotik_presence_bridge_people_home
        below: 1
    actions:
      - action: light.turn_off
        target: {entity_id: all}
```

Welcome home on arrival, using the AP that saw the phone first:

```yaml
automation:
  - alias: Alice arrives
    triggers:
      - trigger: state
        entity_id: device_tracker.alice_presence
        from: not_home
        to: home
    actions:
      - action: notify.mobile_app_alice_phone
        data:
          message: "Welcome home (via {{ state_attr('device_tracker.alice_presence', 'device') }})"
```

Room-level hint from the access point:

```yaml
template:
  - sensor:
      - name: Alice area
        state: >
          {% set ap = state_attr('device_tracker.alice_presence', 'device') %}
          {{ {'ap-living-room': 'Living room', 'ap-garden': 'Garden'}.get(ap, 'Away' if ap is none else ap) }}
```

## Tuning

- **Slow to detect departures?** Lower `presence.consider_home`, enable
  remote logging (disconnects are then immediate), and remove `arp` from
  `presence.sources` if your router keeps entries `reachable` for a long
  time.
- **Flapping while at home?** Phones in deep sleep sometimes drop off Wi-Fi.
  Raise `consider_home` to 5–10 minutes, or keep `bridge_host`/`neighbor` in
  `sources`, since those see wired-side traffic too.
