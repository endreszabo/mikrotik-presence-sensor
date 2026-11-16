package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const sampleYAML = `
log:
  format: json
mqtt:
  broker: tcp://[2001:db8::10]:1883
presence:
  consider_home: 5m
defaults:
  rest:
    username: presence
    password: from-yaml
devices:
  - name: ap1
    host: 2001:db8::2
    kinds: [wifi, wireless]
  - name: router
    host: router.lan
    transport: snmp
    poll_interval: 10s
    kinds: [arp, neighbor]
people:
  - id: alice
    name: Alice
    macs: [AA-BB-CC-DD-EE-01]
`

func TestLoadYAMLWithDefaults(t *testing.T) {
	cfg, err := Load(writeFile(t, "c.yaml", sampleYAML))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Log.Format != "json" || cfg.Log.Level != "info" {
		t.Errorf("log = %+v", cfg.Log)
	}
	if cfg.Presence.ConsiderHome != 5*time.Minute || cfg.Presence.EventTTL != 5*time.Minute {
		t.Errorf("presence = %+v", cfg.Presence)
	}
	ap := cfg.Devices[0]
	if ap.Transport != "rest" || ap.PollInterval != 30*time.Second || ap.REST.Port != 443 ||
		ap.REST.Username != "presence" || ap.REST.Password != "from-yaml" {
		t.Errorf("ap1 not merged with defaults: %+v", ap.DeviceSettings)
	}
	r := cfg.Devices[1]
	if r.Transport != "snmp" || r.PollInterval != 10*time.Second || r.SNMP.Version != "2c" || r.SNMP.Community != "public" {
		t.Errorf("router = %+v", r.DeviceSettings)
	}
	if got := cfg.People[0].MACs[0]; got != "aa:bb:cc:dd:ee:01" {
		t.Errorf("mac not normalized: %q", got)
	}
	if cfg.MQTT.ClientID != "mikrotik-presence-mikrotik_presence" {
		t.Errorf("client id = %q", cfg.MQTT.ClientID)
	}
}

func TestEnvOverridesYAML(t *testing.T) {
	t.Setenv("MPS_LOG_LEVEL", "debug")
	t.Setenv("MPS_MQTT_BROKER", "ssl://[2001:db8::20]:8883")
	t.Setenv("MPS_DEVICES_0_POLL_INTERVAL", "15s")
	t.Setenv("MPS_DEVICES_1_SNMP_COMMUNITY", "s3cret")
	t.Setenv("MPS_PEOPLE_0_MACS", "aa:bb:cc:dd:ee:01,aa:bb:cc:dd:ee:02")
	t.Setenv("MPS_PRESENCE_SOURCES", "wifi,arp")
	cfg, err := Load(writeFile(t, "c.yaml", sampleYAML))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Log.Level != "debug" || cfg.Log.Format != "json" {
		t.Errorf("log = %+v", cfg.Log)
	}
	if cfg.MQTT.Broker != "ssl://[2001:db8::20]:8883" {
		t.Errorf("broker = %q", cfg.MQTT.Broker)
	}
	if cfg.Devices[0].PollInterval != 15*time.Second || cfg.Devices[0].Host != "2001:db8::2" {
		t.Errorf("device 0 = %+v", cfg.Devices[0])
	}
	if cfg.Devices[1].SNMP.Community != "s3cret" {
		t.Errorf("community = %q", cfg.Devices[1].SNMP.Community)
	}
	if len(cfg.People[0].MACs) != 2 || cfg.People[0].Name != "Alice" {
		t.Errorf("person = %+v", cfg.People[0])
	}
	if strings.Join(cfg.Presence.Sources, ",") != "wifi,arp" {
		t.Errorf("sources = %v", cfg.Presence.Sources)
	}
}

func TestEnvOnly(t *testing.T) {
	secret := writeFile(t, "pw", "file-secret\n")
	for k, v := range map[string]string{
		"MPS_MQTT_BROKER":                 "tcp://[::1]:1883",
		"MPS_MQTT_PASSWORD_FILE":          secret,
		"MPS_DEFAULTS_REST_USERNAME":      "presence",
		"MPS_DEFAULTS_REST_PASSWORD_FILE": secret,
		"MPS_DEVICES_0_NAME":              "ap1",
		"MPS_DEVICES_0_HOST":              "2001:db8::2",
		"MPS_DEVICES_1_NAME":              "sw1",
		"MPS_DEVICES_1_HOST":              "192.0.2.3",
		"MPS_DEVICES_1_TRANSPORT":         "snmp",
		"MPS_DEVICES_1_SNMP_VERSION":      "3",
		"MPS_DEVICES_1_SNMP_USERNAME":     "presence",
		"MPS_DEVICES_1_KINDS":             "bridge_host",
		"MPS_PEOPLE_0_ID":                 "bob",
		"MPS_PEOPLE_0_MACS":               "aabb.ccdd.ee03",
		"MPS_SYSLOG_RULES_0_PATTERN":      `presence-seen {mac}`,
		"MPS_SYSLOG_RULES_0_EVENT":        "seen",
	} {
		t.Setenv(k, v)
	}
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MQTT.Password != "file-secret" || cfg.Devices[0].REST.Password != "file-secret" {
		t.Errorf("secrets not read from file: mqtt=%q rest=%q", cfg.MQTT.Password, cfg.Devices[0].REST.Password)
	}
	if len(cfg.Devices) != 2 || cfg.Devices[1].Transport != "snmp" || cfg.Devices[1].SNMP.Version != "3" {
		t.Errorf("devices = %+v", cfg.Devices)
	}
	if !cfg.Devices[0].AutoKinds() {
		t.Errorf("device 0 kinds = %v, want auto", cfg.Devices[0].Kinds)
	}
	if cfg.People[0].MACs[0] != "aa:bb:cc:dd:ee:03" || cfg.People[0].Name != "bob" {
		t.Errorf("person = %+v", cfg.People[0])
	}
	if len(cfg.Syslog.Rules) != 1 || cfg.Syslog.Rules[0].Event != "seen" {
		t.Errorf("rules = %+v", cfg.Syslog.Rules)
	}
	if strings.Contains(cfg.Redacted(), "file-secret") {
		t.Error("Redacted leaks secrets")
	}
}

func TestValidation(t *testing.T) {
	cases := map[string]string{
		"duplicate mac": `
people:
  - {id: a, macs: ["aa:bb:cc:dd:ee:01"]}
  - {id: b, macs: ["AA:BB:CC:DD:EE:01"]}`,
		"bad kind": `
devices: [{name: x, host: ::1, kinds: [wifi6], rest: {username: u}}]
people: [{id: a, macs: ["aa:bb:cc:dd:ee:01"]}]`,
		"dhcp over snmp": `
devices: [{name: x, host: ::1, transport: snmp, kinds: [dhcp]}]
people: [{id: a, macs: ["aa:bb:cc:dd:ee:01"]}]`,
		"bracketed host": `
devices: [{name: x, host: "[::1]", rest: {username: u}}]
people: [{id: a, macs: ["aa:bb:cc:dd:ee:01"]}]`,
		"bad person id": `
people: [{id: "Alice Smith", macs: ["aa:bb:cc:dd:ee:01"]}]`,
		"no people": `log: {level: info}`,
		"unknown key": `
peple: []`,
	}
	for name, y := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeFile(t, "c.yaml", y)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}
