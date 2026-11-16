// Package config defines the configuration tree. Every field carries both a
// yaml tag and an env tag so that one struct is filled from a YAML file first
// and then overlaid by MPS_* environment variables (caarlos0/env).
package config

import "time"

// EnvPrefix is prepended to every environment variable name.
const EnvPrefix = "MPS_"

type Config struct {
	Log      LogConfig      `yaml:"log" envPrefix:"LOG_"`
	Health   HealthConfig   `yaml:"health" envPrefix:"HEALTH_"`
	MQTT     MQTTConfig     `yaml:"mqtt" envPrefix:"MQTT_"`
	Presence PresenceConfig `yaml:"presence" envPrefix:"PRESENCE_"`
	Defaults DeviceSettings `yaml:"defaults" envPrefix:"DEFAULTS_"`
	Devices  []Device       `yaml:"devices" envPrefix:"DEVICES"`
	Syslog   SyslogConfig   `yaml:"syslog" envPrefix:"SYSLOG_"`
	People   []Person       `yaml:"people" envPrefix:"PEOPLE"`
}

type LogConfig struct {
	// Format is "zorp" (default) or "json".
	Format string `yaml:"format" env:"FORMAT"`
	Level  string `yaml:"level" env:"LEVEL"`
}

type HealthConfig struct {
	Listen string `yaml:"listen" env:"LISTEN"`
	// RequireMQTT makes /healthz fail while the broker connection is down.
	RequireMQTT bool `yaml:"require_mqtt" env:"REQUIRE_MQTT"`
	// Metrics serves Prometheus metrics on /metrics of the same listener.
	Metrics bool `yaml:"metrics" env:"METRICS"`
}

type MQTTConfig struct {
	// Broker URL, e.g. tcp://[2001:db8::10]:1883, ssl://broker.lan:8883, ws://…
	Broker               string        `yaml:"broker" env:"BROKER"`
	Username             string        `yaml:"username" env:"USERNAME"`
	Password             string        `yaml:"password" env:"PASSWORD"`
	PasswordFile         string        `yaml:"password_file" env:"PASSWORD_FILE"`
	ClientID             string        `yaml:"client_id" env:"CLIENT_ID"`
	BaseTopic            string        `yaml:"base_topic" env:"BASE_TOPIC"`
	DiscoveryPrefix      string        `yaml:"discovery_prefix" env:"DISCOVERY_PREFIX"`
	NodeID               string        `yaml:"node_id" env:"NODE_ID"`
	Keepalive            time.Duration `yaml:"keepalive" env:"KEEPALIVE"`
	ConnectTimeout       time.Duration `yaml:"connect_timeout" env:"CONNECT_TIMEOUT"`
	MaxReconnectInterval time.Duration `yaml:"max_reconnect_interval" env:"MAX_RECONNECT_INTERVAL"`
	TLS                  TLSConfig     `yaml:"tls" envPrefix:"TLS_"`
}

type TLSConfig struct {
	CAFile             string `yaml:"ca_file" env:"CA_FILE"`
	CertFile           string `yaml:"cert_file" env:"CERT_FILE"`
	KeyFile            string `yaml:"key_file" env:"KEY_FILE"`
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify" env:"INSECURE_SKIP_VERIFY"`
	ServerName         string `yaml:"server_name" env:"SERVER_NAME"`
}

type PresenceConfig struct {
	// ConsiderHome is how long a person stays "home" after the last sighting.
	ConsiderHome time.Duration `yaml:"consider_home" env:"CONSIDER_HOME"`
	// EventTTL is how long a "seen" log event (e.g. a DHCP lease) counts as a
	// sighting. Defaults to ConsiderHome.
	EventTTL time.Duration `yaml:"event_ttl" env:"EVENT_TTL"`
	// Sources lists the kinds that count towards presence.
	Sources []string `yaml:"sources" env:"SOURCES"`
	// ARPStates and NeighborStates list the table entry states that count as
	// a sighting; stale entries linger long after a phone has left.
	ARPStates      []string `yaml:"arp_states" env:"ARP_STATES"`
	NeighborStates []string `yaml:"neighbor_states" env:"NEIGHBOR_STATES"`
	// StaleAfterFailedPolls drops a device's last snapshot after that many
	// consecutive failed polls so people do not get stuck at home.
	StaleAfterFailedPolls int `yaml:"stale_after_failed_polls" env:"STALE_AFTER_FAILED_POLLS"`
}

// DeviceSettings are the per-device knobs; the top level "defaults" block uses
// the same type and fills in whatever a device leaves unset.
type DeviceSettings struct {
	// Transport is "rest" or "snmp".
	Transport string `yaml:"transport" env:"TRANSPORT"`
	// Kinds to poll; "auto" probes the device, "none" disables polling (the
	// device then only feeds syslog events).
	Kinds        []string      `yaml:"kinds" env:"KINDS"`
	PollInterval time.Duration `yaml:"poll_interval" env:"POLL_INTERVAL"`
	Timeout      time.Duration `yaml:"timeout" env:"TIMEOUT"`
	REST         RESTConfig    `yaml:"rest" envPrefix:"REST_"`
	SNMP         SNMPConfig    `yaml:"snmp" envPrefix:"SNMP_"`
}

type Device struct {
	Name string `yaml:"name" env:"NAME"`
	// Host is an IPv6/IPv4 literal or a DNS name.
	Host string `yaml:"host" env:"HOST"`
	// SyslogSources are extra source addresses this device may send logs
	// from; Host is always included.
	SyslogSources  []string `yaml:"syslog_sources" env:"SYSLOG_SOURCES"`
	DeviceSettings `yaml:",inline"`
}

type RESTConfig struct {
	Port               int    `yaml:"port" env:"PORT"`
	Username           string `yaml:"username" env:"USERNAME"`
	Password           string `yaml:"password" env:"PASSWORD"`
	PasswordFile       string `yaml:"password_file" env:"PASSWORD_FILE"`
	CAFile             string `yaml:"ca_file" env:"CA_FILE"`
	InsecureSkipVerify *bool  `yaml:"insecure_skip_verify" env:"INSECURE_SKIP_VERIFY"`
	ServerName         string `yaml:"server_name" env:"SERVER_NAME"`
}

type SNMPConfig struct {
	Port int `yaml:"port" env:"PORT"`
	// Version is "2c" or "3".
	Version        string `yaml:"version" env:"VERSION"`
	Community      string `yaml:"community" env:"COMMUNITY"`
	CommunityFile  string `yaml:"community_file" env:"COMMUNITY_FILE"`
	Retries        int    `yaml:"retries" env:"RETRIES"`
	MaxRepetitions uint32 `yaml:"max_repetitions" env:"MAX_REPETITIONS"`
	// SecurityLevel is noAuthNoPriv, authNoPriv or authPriv.
	SecurityLevel    string `yaml:"security_level" env:"SECURITY_LEVEL"`
	Username         string `yaml:"username" env:"USERNAME"`
	AuthProtocol     string `yaml:"auth_protocol" env:"AUTH_PROTOCOL"`
	AuthPassword     string `yaml:"auth_password" env:"AUTH_PASSWORD"`
	AuthPasswordFile string `yaml:"auth_password_file" env:"AUTH_PASSWORD_FILE"`
	PrivProtocol     string `yaml:"priv_protocol" env:"PRIV_PROTOCOL"`
	PrivPassword     string `yaml:"priv_password" env:"PRIV_PASSWORD"`
	PrivPasswordFile string `yaml:"priv_password_file" env:"PRIV_PASSWORD_FILE"`
	ContextName      string `yaml:"context_name" env:"CONTEXT_NAME"`
}

type SyslogConfig struct {
	Enabled bool `yaml:"enabled" env:"ENABLED"`
	// UDPListen and TCPListen are host:port; empty disables that listener.
	UDPListen string `yaml:"udp_listen" env:"UDP_LISTEN"`
	TCPListen string `yaml:"tcp_listen" env:"TCP_LISTEN"`
	// AllowedSources are extra CIDRs/addresses allowed to send logs on top of
	// the configured devices. "any" disables the check.
	AllowedSources      []string     `yaml:"allowed_sources" env:"ALLOWED_SOURCES"`
	DisableDefaultRules bool         `yaml:"disable_default_rules" env:"DISABLE_DEFAULT_RULES"`
	Rules               []SyslogRule `yaml:"rules" envPrefix:"RULES"`
}

// SyslogRule turns a matching log line into a presence event. Pattern is a Go
// regexp; "{mac}" expands to a MAC address group. Named groups: mac
// (required), iface, ip, event.
type SyslogRule struct {
	Name    string `yaml:"name" env:"NAME"`
	Pattern string `yaml:"pattern" env:"PATTERN"`
	// Event is join, leave or seen; empty derives it from the "event" group.
	Event string `yaml:"event" env:"EVENT"`
	Kind  string `yaml:"kind" env:"KIND"`
}

type Person struct {
	ID   string   `yaml:"id" env:"ID"`
	Name string   `yaml:"name" env:"NAME"`
	MACs []string `yaml:"macs" env:"MACS"`
	Icon string   `yaml:"icon" env:"ICON"`
}

// Default returns the built-in defaults, applied before the YAML file and the
// environment. They live here rather than in envDefault tags so environment
// parsing never clobbers values coming from YAML.
func Default() Config {
	return Config{
		Log:    LogConfig{Format: "zorp", Level: "info"},
		Health: HealthConfig{Listen: "[::]:8080", RequireMQTT: true, Metrics: true},
		MQTT: MQTTConfig{
			Broker:               "tcp://[::1]:1883",
			BaseTopic:            "mikrotik-presence",
			DiscoveryPrefix:      "homeassistant",
			NodeID:               "mikrotik_presence",
			Keepalive:            30 * time.Second,
			ConnectTimeout:       10 * time.Second,
			MaxReconnectInterval: time.Minute,
		},
		Presence: PresenceConfig{
			ConsiderHome:          3 * time.Minute,
			Sources:               []string{"wifi", "wireless", "capsman", "bridge_host", "neighbor", "arp", "dhcp"},
			ARPStates:             []string{"reachable", "delay", "probe", "dynamic"},
			NeighborStates:        []string{"reachable", "delay", "probe"},
			StaleAfterFailedPolls: 3,
		},
		Defaults: DeviceSettings{
			Transport:    "rest",
			Kinds:        []string{"auto"},
			PollInterval: 30 * time.Second,
			Timeout:      5 * time.Second,
			REST:         RESTConfig{Port: 443},
			SNMP: SNMPConfig{
				Port:           161,
				Version:        "2c",
				Community:      "public",
				Retries:        1,
				MaxRepetitions: 25,
				SecurityLevel:  "authPriv",
				AuthProtocol:   "SHA256",
				PrivProtocol:   "AES",
			},
		},
		Syslog: SyslogConfig{
			Enabled:   true,
			UDPListen: "[::]:5514",
			TCPListen: "[::]:5514",
		},
	}
}
