package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/caarlos0/env/v11"
	"github.com/goccy/go-yaml"

	"github.com/endreszabo/mikrotik-presence-sensor/internal/macaddr"
	"github.com/endreszabo/mikrotik-presence-sensor/internal/mikrotik"
)

// Load builds the effective configuration: built-in defaults, then the YAML
// file at path (if not empty), then MPS_* environment variables. Device
// settings are merged with the defaults block, secret files are read and the
// result is validated.
func Load(path string) (*Config, error) {
	cfg := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading config: %w", err)
		}
		if err := yaml.UnmarshalWithOptions(raw, &cfg, yaml.Strict()); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", path, err)
		}
	}
	if err := env.ParseWithOptions(&cfg, env.Options{Prefix: EnvPrefix}); err != nil {
		return nil, fmt.Errorf("parsing environment: %w", err)
	}
	if err := cfg.finalize(); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// finalize merges device settings with defaults, reads secret files and
// normalizes values in place.
func (c *Config) finalize() error {
	var errs []error
	readSecret := func(dst *string, file, what string) {
		if file == "" {
			return
		}
		b, err := os.ReadFile(file)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", what, err))
			return
		}
		*dst = strings.TrimRight(string(b), "\r\n")
	}
	readSecret(&c.MQTT.Password, c.MQTT.PasswordFile, "mqtt.password_file")

	if c.Presence.EventTTL == 0 {
		c.Presence.EventTTL = c.Presence.ConsiderHome
	}
	if c.MQTT.ClientID == "" {
		c.MQTT.ClientID = "mikrotik-presence-" + c.MQTT.NodeID
	}

	for i := range c.Devices {
		d := &c.Devices[i]
		d.DeviceSettings = mergeSettings(d.DeviceSettings, c.Defaults)
		d.Transport = strings.ToLower(d.Transport)
		what := fmt.Sprintf("devices[%d] (%s)", i, d.Name)
		readSecret(&d.REST.Password, d.REST.PasswordFile, what+" rest.password_file")
		readSecret(&d.SNMP.Community, d.SNMP.CommunityFile, what+" snmp.community_file")
		readSecret(&d.SNMP.AuthPassword, d.SNMP.AuthPasswordFile, what+" snmp.auth_password_file")
		readSecret(&d.SNMP.PrivPassword, d.SNMP.PrivPasswordFile, what+" snmp.priv_password_file")
	}

	for i := range c.People {
		p := &c.People[i]
		for j, m := range p.MACs {
			n, err := macaddr.Normalize(m)
			if err != nil {
				errs = append(errs, fmt.Errorf("people[%d] (%s): mac %q: %w", i, p.ID, m, err))
				continue
			}
			p.MACs[j] = n
		}
		if p.Name == "" {
			p.Name = p.ID
		}
	}
	return errors.Join(errs...)
}

func mergeSettings(d, def DeviceSettings) DeviceSettings {
	or := func(v, fallback string) string {
		if v == "" {
			return fallback
		}
		return v
	}
	if d.Transport == "" {
		d.Transport = def.Transport
	}
	if len(d.Kinds) == 0 {
		d.Kinds = slices.Clone(def.Kinds)
	}
	if d.PollInterval == 0 {
		d.PollInterval = def.PollInterval
	}
	if d.Timeout == 0 {
		d.Timeout = def.Timeout
	}

	r, dr := &d.REST, def.REST
	if r.Port == 0 {
		r.Port = dr.Port
	}
	r.Username = or(r.Username, dr.Username)
	if r.Password == "" && r.PasswordFile == "" {
		r.Password, r.PasswordFile = dr.Password, dr.PasswordFile
	}
	r.CAFile = or(r.CAFile, dr.CAFile)
	r.ServerName = or(r.ServerName, dr.ServerName)
	if r.InsecureSkipVerify == nil {
		r.InsecureSkipVerify = dr.InsecureSkipVerify
	}

	s, ds := &d.SNMP, def.SNMP
	if s.Port == 0 {
		s.Port = ds.Port
	}
	s.Version = or(s.Version, ds.Version)
	if s.Community == "" && s.CommunityFile == "" {
		s.Community, s.CommunityFile = ds.Community, ds.CommunityFile
	}
	if s.Retries == 0 {
		s.Retries = ds.Retries
	}
	if s.MaxRepetitions == 0 {
		s.MaxRepetitions = ds.MaxRepetitions
	}
	s.SecurityLevel = or(s.SecurityLevel, ds.SecurityLevel)
	s.Username = or(s.Username, ds.Username)
	s.AuthProtocol = or(s.AuthProtocol, ds.AuthProtocol)
	if s.AuthPassword == "" && s.AuthPasswordFile == "" {
		s.AuthPassword, s.AuthPasswordFile = ds.AuthPassword, ds.AuthPasswordFile
	}
	s.PrivProtocol = or(s.PrivProtocol, ds.PrivProtocol)
	if s.PrivPassword == "" && s.PrivPasswordFile == "" {
		s.PrivPassword, s.PrivPasswordFile = ds.PrivPassword, ds.PrivPasswordFile
	}
	s.ContextName = or(s.ContextName, ds.ContextName)
	return d
}

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Validate checks the finalized configuration for consistency.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if c.Log.Format != "zorp" && c.Log.Format != "json" {
		add("log.format must be zorp or json, got %q", c.Log.Format)
	}
	if c.MQTT.Broker == "" {
		add("mqtt.broker is required")
	}
	if !idRe.MatchString(c.MQTT.NodeID) {
		add("mqtt.node_id %q must match %s", c.MQTT.NodeID, idRe)
	}
	if c.Presence.ConsiderHome <= 0 {
		add("presence.consider_home must be positive")
	}
	for _, s := range c.Presence.Sources {
		if _, err := mikrotik.ParseKind(s); err != nil {
			add("presence.sources: %v", err)
		}
	}

	names := map[string]bool{}
	for i, d := range c.Devices {
		where := fmt.Sprintf("devices[%d]", i)
		if d.Name == "" {
			add("%s: name is required", where)
		} else if names[d.Name] {
			add("%s: duplicate device name %q", where, d.Name)
		}
		names[d.Name] = true
		where = fmt.Sprintf("device %q", d.Name)
		if d.Host == "" {
			add("%s: host is required", where)
		} else if strings.ContainsAny(d.Host, "[]/") {
			add("%s: host %q must be a bare address or name (no brackets, no URL)", where, d.Host)
		}
		for _, s := range d.SyslogSources {
			if _, err := netip.ParseAddr(s); err != nil {
				add("%s: syslog_sources: %v", where, err)
			}
		}
		if d.PollInterval <= 0 {
			add("%s: poll_interval must be positive", where)
		}
		kinds, err := d.ParsedKinds()
		if err != nil {
			add("%s: %v", where, err)
		}
		if len(kinds) == 0 && !d.PollingDisabled() && !d.AutoKinds() {
			add("%s: kinds is empty", where)
		}
		switch d.Transport {
		case "rest":
			if !d.PollingDisabled() && d.REST.Username == "" {
				add("%s: rest.username is required", where)
			}
		case "snmp":
			if slices.Contains(kinds, mikrotik.KindDHCP) {
				add("%s: kind dhcp is only available via the rest transport", where)
			}
			switch d.SNMP.Version {
			case "2c":
			case "3":
				if d.SNMP.Username == "" {
					add("%s: snmp.username is required for SNMPv3", where)
				}
			default:
				add("%s: snmp.version must be 2c or 3, got %q", where, d.SNMP.Version)
			}
		default:
			add("%s: transport must be rest or snmp, got %q", where, d.Transport)
		}
	}

	for _, s := range c.Syslog.AllowedSources {
		if s == "any" {
			continue
		}
		if _, err := netip.ParsePrefix(s); err == nil {
			continue
		}
		if _, err := netip.ParseAddr(s); err != nil {
			add("syslog.allowed_sources: %q is neither an address nor a CIDR", s)
		}
	}
	if c.Syslog.Enabled {
		for _, l := range []string{c.Syslog.UDPListen, c.Syslog.TCPListen} {
			if l == "" {
				continue
			}
			if _, _, err := net.SplitHostPort(l); err != nil {
				add("syslog listen address %q: %v", l, err)
			}
		}
	}
	for i, r := range c.Syslog.Rules {
		if r.Pattern == "" {
			add("syslog.rules[%d]: pattern is required", i)
		}
		if r.Event != "" && r.Event != "join" && r.Event != "leave" && r.Event != "seen" {
			add("syslog.rules[%d]: event must be join, leave or seen", i)
		}
		if r.Kind != "" {
			if _, err := mikrotik.ParseKind(r.Kind); err != nil {
				add("syslog.rules[%d]: %v", i, err)
			}
		}
	}

	ids := map[string]bool{}
	owner := map[string]string{}
	for i, p := range c.People {
		if !idRe.MatchString(p.ID) {
			add("people[%d]: id %q must match %s", i, p.ID, idRe)
		} else if ids[p.ID] {
			add("people[%d]: duplicate id %q", i, p.ID)
		}
		ids[p.ID] = true
		if len(p.MACs) == 0 {
			add("person %q: at least one mac is required", p.ID)
		}
		for _, m := range p.MACs {
			if o, ok := owner[m]; ok && o != p.ID {
				add("person %q: mac %s is already assigned to %q", p.ID, m, o)
			}
			owner[m] = p.ID
		}
	}
	if len(c.People) == 0 {
		add("no people configured")
	}
	return errors.Join(errs...)
}

// AutoKinds reports whether the device kinds should be probed.
func (d Device) AutoKinds() bool {
	return len(d.Kinds) == 1 && d.Kinds[0] == "auto"
}

// PollingDisabled reports whether the device only feeds syslog events.
func (d Device) PollingDisabled() bool {
	return len(d.Kinds) == 1 && d.Kinds[0] == "none"
}

// ParsedKinds returns the explicitly configured kinds (nil for auto/none).
func (d Device) ParsedKinds() ([]mikrotik.Kind, error) {
	if d.AutoKinds() || d.PollingDisabled() {
		return nil, nil
	}
	var out []mikrotik.Kind
	for _, s := range d.Kinds {
		k, err := mikrotik.ParseKind(s)
		if err != nil {
			return nil, fmt.Errorf("kinds: %w (use auto, none or a list of kinds)", err)
		}
		out = append(out, k)
	}
	return out, nil
}

// Redacted returns the configuration as YAML with secrets masked, suitable
// for debug logging and -check-config.
func (c *Config) Redacted() string {
	cp := *c
	mask := func(s *string) {
		if *s != "" {
			*s = "***"
		}
	}
	mask(&cp.MQTT.Password)
	mask(&cp.Defaults.REST.Password)
	mask(&cp.Defaults.SNMP.Community)
	mask(&cp.Defaults.SNMP.AuthPassword)
	mask(&cp.Defaults.SNMP.PrivPassword)
	cp.Devices = slices.Clone(c.Devices)
	for i := range cp.Devices {
		mask(&cp.Devices[i].REST.Password)
		mask(&cp.Devices[i].SNMP.Community)
		mask(&cp.Devices[i].SNMP.AuthPassword)
		mask(&cp.Devices[i].SNMP.PrivPassword)
	}
	out, err := yaml.MarshalWithOptions(cp, yaml.UseLiteralStyleIfMultiline(true))
	if err != nil {
		return err.Error()
	}
	return string(out)
}
