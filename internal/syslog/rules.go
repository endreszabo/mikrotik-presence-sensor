// Package syslog receives RouterOS remote log messages over UDP and TCP and
// turns association, disassociation and DHCP lines into presence events.
package syslog

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/endreszabo/mikrotik-presence-sensor/internal/config"
	"github.com/endreszabo/mikrotik-presence-sensor/internal/macaddr"
	"github.com/endreszabo/mikrotik-presence-sensor/internal/mikrotik"
	"github.com/endreszabo/mikrotik-presence-sensor/internal/presence"
)

// macPattern is substituted for "{mac}" in rule patterns.
const macPattern = `(?P<mac>(?:[0-9A-Fa-f]{2}[:-]){5}[0-9A-Fa-f]{2})`

// DefaultRules match RouterOS 6/7 log lines. Order matters: the first match
// wins. Examples:
//
//	AA:BB:CC:DD:EE:FF@wlan1: connected, signal strength -62          (wireless)
//	AA:BB:CC:DD:EE:FF@wlan1: disconnected, received deauth: ...      (wireless)
//	AA:BB:CC:DD:EE:FF@wifi2 connected, signal strength -58           (wifi, CAPsMAN)
//	AA:BB:CC:DD:EE:FF@wifi2 disconnected, connection lost, ...       (wifi, CAPsMAN)
//	dhcp1 assigned 192.0.2.10 for AA:BB:CC:DD:EE:FF phone            (dhcp)
//	dhcp1 deassigned 192.0.2.10 for AA:BB:CC:DD:EE:FF phone          (dhcp)
func DefaultRules() []config.SyslogRule {
	return []config.SyslogRule{
		{Name: "wireless", Pattern: `{mac}@(?P<iface>[^\s:@]+): (?P<event>connected|disconnected)\b`, Kind: "wireless"},
		{Name: "wifi", Pattern: `{mac}@(?P<iface>[^\s:@]+),? (?P<event>connected|disconnected)\b`, Kind: "wifi"},
		{Name: "dhcp-deassigned", Pattern: `\bdeassigned (?P<ip>\S+) (?:for|from) {mac}`, Event: "leave", Kind: "dhcp"},
		{Name: "dhcp-assigned", Pattern: `\bassigned (?P<ip>\S+) (?:for|to) {mac}`, Event: "seen", Kind: "dhcp"},
	}
}

// Rule is a compiled config.SyslogRule.
type Rule struct {
	Name  string
	re    *regexp.Regexp
	event presence.EventType
	kind  mikrotik.Kind
}

// Match is the result of a rule matching a line.
type Match struct {
	Rule      string
	Event     presence.EventType
	Kind      mikrotik.Kind
	MAC       string
	Interface string
	IP        string
}

// CompileRules compiles rules, expanding "{mac}".
func CompileRules(rules []config.SyslogRule) ([]Rule, error) {
	out := make([]Rule, 0, len(rules))
	for i, r := range rules {
		name := r.Name
		if name == "" {
			name = fmt.Sprintf("rule%d", i)
		}
		re, err := regexp.Compile(strings.ReplaceAll(r.Pattern, "{mac}", macPattern))
		if err != nil {
			return nil, fmt.Errorf("syslog rule %s: %w", name, err)
		}
		if re.SubexpIndex("mac") < 0 {
			return nil, fmt.Errorf("syslog rule %s: pattern needs a (?P<mac>...) group or {mac}", name)
		}
		if r.Event == "" && re.SubexpIndex("event") < 0 {
			return nil, fmt.Errorf("syslog rule %s: set event or add an (?P<event>...) group", name)
		}
		out = append(out, Rule{Name: name, re: re, event: presence.EventType(r.Event), kind: mikrotik.Kind(r.Kind)})
	}
	return out, nil
}

// eventWords maps captured "event" group values to event types.
var eventWords = map[string]presence.EventType{
	"connected": presence.EventJoin, "associated": presence.EventJoin, "join": presence.EventJoin, "up": presence.EventJoin,
	"disconnected": presence.EventLeave, "disassociated": presence.EventLeave, "leave": presence.EventLeave, "down": presence.EventLeave,
	"assigned": presence.EventSeen, "seen": presence.EventSeen,
	"deassigned": presence.EventLeave,
}

// MatchLine runs the rules against a message and returns the first match.
func MatchLine(rules []Rule, text string) (Match, bool) {
	for _, r := range rules {
		m := r.re.FindStringSubmatch(text)
		if m == nil {
			continue
		}
		group := func(name string) string {
			if i := r.re.SubexpIndex(name); i >= 0 {
				return m[i]
			}
			return ""
		}
		mac, err := macaddr.Normalize(group("mac"))
		if err != nil {
			continue
		}
		ev := r.event
		if ev == "" {
			var ok bool
			if ev, ok = eventWords[strings.ToLower(group("event"))]; !ok {
				continue
			}
		}
		return Match{Rule: r.Name, Event: ev, Kind: r.kind, MAC: mac, Interface: group("iface"), IP: group("ip")}, true
	}
	return Match{}, false
}

var (
	priRe     = regexp.MustCompile(`^<\d{1,3}>`)
	rfc5424Re = regexp.MustCompile(`^\d{1,2} \S+ \S+ \S+ \S+ \S+ (?:-|(?:\[(?:[^\]\\]|\\.)*\])+) ?`)
	rfc3164Re = regexp.MustCompile(`^[A-Z][a-z]{2} [ 0-9]\d \d\d:\d\d:\d\d `)
)

// StripHeader removes the syslog PRI and RFC 5424 / RFC 3164 headers,
// returning what RouterOS logged (possibly prefixed by host and topics).
func StripHeader(line string) string {
	line = strings.TrimRight(line, "\r\n\x00")
	line = priRe.ReplaceAllString(line, "")
	if loc := rfc5424Re.FindStringIndex(line); loc != nil {
		return line[loc[1]:]
	}
	return rfc3164Re.ReplaceAllString(line, "")
}
