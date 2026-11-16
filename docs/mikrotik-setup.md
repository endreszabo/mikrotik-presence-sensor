# MikroTik setup

All snippets below are also in [`examples/routeros/`](../examples/routeros).
They use `2001:db8:0:10::5` as the address of the host that runs the sensor.

## Which transport?

| | REST (HTTPS) | SNMP |
|---|---|---|
| RouterOS | 7.1+ | 6 and 7, and other vendors' switches for ARP/ND/FDB |
| `wifi` package registration table | yes | best effort only (see below) |
| legacy `wireless`, CAPsMAN | yes | yes |
| ARP, IPv6 neighbors, bridge hosts | yes | yes (standard MIBs) |
| DHCP leases | yes | no |
| Details | SSID, band, signal, interface names | signal, interface names (CAPsMAN: SSID) |
| Security | TLS + per-user policy | v3 authPriv; v2c is cleartext |

Use REST wherever you can, and SNMP for older or non-RouterOS gear. You can
mix them per device.

> The public MIKROTIK-MIB only defines registration tables for the legacy
> `wireless` package (`mtxrWlRtabTable`) and legacy CAPsMAN
> (`mtxrWlCMRtabTable`). For `kind: wifi` over SNMP, the sensor walks
> `mtxrWlRtabTable` in case your RouterOS version populates it for `wifi`
> interfaces. It doesn't probe this automatically; you have to request it
> explicitly. Prefer REST for `wifi` APs.

## 1. A read-only user (REST)

```routeros
/user group add name=presence policy=read,api,rest-api,!write,!policy,!sensitive comment="mikrotik-presence-sensor"
/user add name=presence group=presence password="CHANGE-ME" address=2001:db8:0:10::5/128
```

The `address` restriction means the account works only from the sensor's
host.

## 2. HTTPS for the REST API

RouterOS serves `/rest` only over the `www-ssl` service, which needs a
certificate. A small private CA works well:

```routeros
/certificate add name=presence-ca common-name=presence-ca key-usage=key-cert-sign,crl-sign days-valid=3650
/certificate sign presence-ca
/certificate export-certificate presence-ca export-passphrase=""

/certificate add name=https-ap1 common-name=ap1.home.arpa \
    subject-alt-name=DNS:ap1.home.arpa,IP:2001:db8:0:10::2 \
    key-usage=digital-signature,key-encipherment,tls-server days-valid=825
/certificate sign https-ap1 ca=presence-ca
/ip service set www-ssl certificate=https-ap1 tls-version=only-1.2 disabled=no address=2001:db8:0:10::5/128
```

Copy the exported `presence-ca.crt` to the sensor host. Then set:

```yaml
defaults:
  rest:
    ca_file: /config/presence-ca.crt
```

If you connect by IP address and the certificate holds only a DNS name, also
set `rest.server_name: ap1.home.arpa` on that device. For a first test,
`rest.insecure_skip_verify: true` gets you running. Replace it later.

To check the setup from the sensor host:

```sh
curl --cacert presence-ca.crt -u presence:CHANGE-ME \
  'https://[2001:db8:0:10::2]/rest/interface/wifi/registration-table?.proplist=mac-address,interface'
```

## 3. SNMP (alternative)

SNMPv3 uses authentication and encryption. The RouterOS community name is the
v3 user name:

```routeros
/snmp community add name=presence addresses=2001:db8:0:10::5/128 security=private \
    authentication-protocol=SHA256 authentication-password="CHANGE-ME-auth" \
    encryption-protocol=AES encryption-password="CHANGE-ME-priv" \
    read-access=yes write-access=no
/snmp set enabled=yes
```

Matching configuration on the sensor:

```yaml
- name: switch-office
  host: 2001:db8:0:10::3
  transport: snmp
  kinds: [bridge_host]
  snmp:
    version: "3"
    username: presence
    auth_protocol: SHA256
    auth_password_file: /run/secrets/snmp_auth
    priv_protocol: AES
    priv_password_file: /run/secrets/snmp_priv
```

Older RouterOS releases offer only MD5 and SHA1 here. In that case use
`authentication-protocol=SHA1` on the device and `auth_protocol: SHA` in the
sensor.

To check with net-snmp:

```sh
snmpbulkwalk -v3 -l authPriv -u presence -a SHA-256 -A CHANGE-ME-auth -x AES -X CHANGE-ME-priv \
  udp6:[2001:db8:0:10::3] 1.3.6.1.2.1.17.7.1.2.2.1.2
```

## 4. Remote logging (real-time events)

```routeros
/system logging action add name=presence target=remote remote=2001:db8:0:10::5 remote-port=5514 \
    bsd-syslog=yes syslog-facility=daemon
/system logging add topics=wireless,info action=presence
/system logging add topics=caps,info action=presence
/system logging add topics=dhcp,info action=presence      # on the DHCP server
```

The sensor accepts RFC 3164 (`bsd-syslog=yes`), RFC 5424, and RouterOS's
plain format, over UDP or TCP on port 5514. If your RouterOS version supports
TCP remote logging, you can switch to it with no change on the sensor side.

The sensor identifies the sender by source address. Configured device hosts
are allowed automatically. If a device logs from a different address than the
one you poll (for example IPv4 or a loopback), list that address under the
device's `syslog_sources`.

## 5. Interplay

- A **connect** event marks the person home immediately, then triggers a poll
  of that AP to fill in signal and SSID.
- A **disconnect** event removes the sighting and triggers a re-poll. If the
  phone has roamed to another AP, that AP's connect event (or poll) keeps the
  person home. If nothing sees the phone for `consider_home`, the person
  becomes `not_home`.
- If a log message is lost, nothing breaks. The next regular poll is
  authoritative and corrects the state.
