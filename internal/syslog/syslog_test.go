package syslog

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/endreszabo/mikrotik-presence-sensor/internal/config"
	"github.com/endreszabo/mikrotik-presence-sensor/internal/mikrotik"
	"github.com/endreszabo/mikrotik-presence-sensor/internal/presence"
)

func TestStripHeader(t *testing.T) {
	for in, want := range map[string]string{
		"<30>Nov 16 22:01:02 ap1 wireless,info AA:BB:CC:DD:EE:01@wifi1 connected":                                   "ap1 wireless,info AA:BB:CC:DD:EE:01@wifi1 connected",
		"<30>1 2026-11-16T22:01:02.000+01:00 ap1 wifi - - - AA:BB:CC:DD:EE:01@wifi1 connected":                      "AA:BB:CC:DD:EE:01@wifi1 connected",
		`<30>1 2026-11-16T22:01:02Z ap1 wifi - - [meta x="1"][a b="\]"] AA:BB:CC:DD:EE:01@wifi1 connected` + "\r\n": "AA:BB:CC:DD:EE:01@wifi1 connected",
		"wireless,info AA:BB:CC:DD:EE:01@wlan1: connected":                                                          "wireless,info AA:BB:CC:DD:EE:01@wlan1: connected",
	} {
		if got := StripHeader(in); got != want {
			t.Errorf("StripHeader(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

func TestDefaultRules(t *testing.T) {
	rules, err := CompileRules(DefaultRules())
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		line  string
		event presence.EventType
		kind  mikrotik.Kind
		iface string
		ip    string
	}{
		{"wireless,info AA:BB:CC:DD:EE:01@wlan1: connected, signal strength -62", presence.EventJoin, mikrotik.KindWireless, "wlan1", ""},
		{"wireless,info AA:BB:CC:DD:EE:01@wlan2: disconnected, received deauth: sending station leaving (3)", presence.EventLeave, mikrotik.KindWireless, "wlan2", ""},
		{"wireless,info AA:BB:CC:DD:EE:01@wifi2 connected, signal strength -58", presence.EventJoin, mikrotik.KindWifi, "wifi2", ""},
		{"wireless,info AA:BB:CC:DD:EE:01@cap-wifi1 disconnected, connection lost, signal strength -82", presence.EventLeave, mikrotik.KindWifi, "cap-wifi1", ""},
		{"dhcp,info dhcp1 assigned 192.0.2.10 for AA:BB:CC:DD:EE:01 Pixel-8", presence.EventSeen, mikrotik.KindDHCP, "", "192.0.2.10"},
		{"dhcp,info dhcp1 deassigned 192.0.2.10 for AA:BB:CC:DD:EE:01 Pixel-8", presence.EventLeave, mikrotik.KindDHCP, "", "192.0.2.10"},
	}
	for _, c := range cases {
		m, ok := MatchLine(rules, c.line)
		if !ok {
			t.Errorf("no match for %q", c.line)
			continue
		}
		if m.Event != c.event || m.Kind != c.kind || m.Interface != c.iface || m.IP != c.ip || m.MAC != "aa:bb:cc:dd:ee:01" {
			t.Errorf("%q => %+v", c.line, m)
		}
	}
	for _, line := range []string{
		"system,info user admin logged in from 2001:db8::5 via winbox",
		"wireless,info AA:BB:CC:DD:EE:01@wifi1 reassociating",
	} {
		if m, ok := MatchLine(rules, line); ok {
			t.Errorf("unexpected match for %q: %+v", line, m)
		}
	}
}

func TestCustomRule(t *testing.T) {
	rules, err := CompileRules([]config.SyslogRule{{Name: "script", Pattern: `presence (?P<event>seen|leave) {mac}`}})
	if err != nil {
		t.Fatal(err)
	}
	m, ok := MatchLine(rules, "script,info presence seen 02-00-00-00-00-01")
	if !ok || m.Event != presence.EventSeen || m.MAC != "02:00:00:00:00:01" || m.Kind != "" {
		t.Fatalf("match = %+v %v", m, ok)
	}
	for _, bad := range []config.SyslogRule{{Pattern: `no mac here`, Event: "seen"}, {Pattern: `{mac}`}, {Pattern: `(`}} {
		if _, err := CompileRules([]config.SyslogRule{bad}); err == nil {
			t.Errorf("rule %+v accepted", bad)
		}
	}
}

func TestReadFrame(t *testing.T) {
	long := strings.Repeat("x", maxMessage+100)
	counted := "<30>octet counted\nwith newline"
	r := bufio.NewReaderSize(strings.NewReader(fmt.Sprintf("%d %s", len(counted), counted)+
		"<30>plain line\r\n"+long+"\nlast\x00\n"), maxMessage)
	var got []string
	for {
		f, err := readFrame(r)
		if err != nil {
			break
		}
		got = append(got, f)
	}
	if len(got) != 4 || got[0] != counted || got[1] != "<30>plain line" ||
		len(got[2]) != maxMessage || got[3] != "last" {
		t.Fatalf("frames = %d %q", len(got), got[:min(len(got), 2)])
	}
}

type collector struct {
	mu  sync.Mutex
	evs []presence.Event
}

func (c *collector) handle(_ context.Context, ev presence.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evs = append(c.evs, ev)
}

func (c *collector) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.evs)
}

func startServer(t *testing.T, opts Options) (*Server, *collector) {
	t.Helper()
	rules, _ := CompileRules(DefaultRules())
	opts.Rules = rules
	opts.UDPListen, opts.TCPListen = "[::1]:0", "[::1]:0"
	c := &collector{}
	s := New(opts, c.handle, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	})
	select {
	case <-s.ready:
	case err := <-done:
		t.Skipf("cannot listen on ::1: %v", err)
	}
	return s, c
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestServerUDPAndTCP(t *testing.T) {
	s, c := startServer(t, Options{Sources: []Source{{Device: "ap1", Host: "::1"}}})

	u, err := net.Dial("udp", s.udpAddr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	fmt.Fprint(u, "<30>Nov 16 22:01:02 ap1 wireless,info AA:BB:CC:DD:EE:01@wifi1 connected, signal strength -60")
	waitFor(t, func() bool { return c.len() == 1 })

	tc, err := net.Dial("tcp", s.tcpAddr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	msg := "<30>wireless,info AA:BB:CC:DD:EE:01@wifi1 disconnected, connection lost"
	fmt.Fprintf(tc, "%d %s", len(msg), msg)
	fmt.Fprint(tc, "<30>system,info something unrelated\n")
	fmt.Fprint(tc, "<30>dhcp,info dhcp1 assigned 192.0.2.9 for AA:BB:CC:DD:EE:02\n")
	waitFor(t, func() bool { return c.len() == 3 })

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.evs[0].Device != "ap1" || c.evs[0].Type != presence.EventJoin || c.evs[1].Type != presence.EventLeave ||
		c.evs[2].Kind != mikrotik.KindDHCP {
		t.Errorf("events = %+v", c.evs)
	}
	if st := s.Stats(); st.Received != 4 || st.Matched != 3 {
		t.Errorf("stats = %+v", st)
	}
}

func TestServerRejectsUnknownSources(t *testing.T) {
	s, c := startServer(t, Options{Sources: []Source{{Device: "ap1", Host: "2001:db8::1"}}})
	u, err := net.Dial("udp", s.udpAddr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	fmt.Fprint(u, "AA:BB:CC:DD:EE:01@wifi1 connected")
	waitFor(t, func() bool { return s.Stats().Dropped == 1 })
	if c.len() != 0 {
		t.Fatal("event from unauthorized source accepted")
	}
}

func TestAttributeAllowedPrefix(t *testing.T) {
	s := New(Options{Allowed: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}}, nil, slog.New(slog.DiscardHandler))
	if dev, ok := s.attribute(netip.MustParseAddr("::ffff:192.0.2.7")); !ok || dev != "192.0.2.7" {
		t.Errorf("mapped address = %q %v", dev, ok)
	}
	if _, ok := s.attribute(netip.MustParseAddr("198.51.100.1")); ok {
		t.Error("address outside prefix allowed")
	}
}

func TestAttributeFirstDeviceWins(t *testing.T) {
	s := New(Options{Sources: []Source{
		{Device: "router-rest", Host: "2001:db8::1"},
		{Device: "router-snmp", Host: "2001:db8::1"},
		{Device: "ap", Host: "2001:db8::2", Extra: []string{"192.0.2.2"}},
	}}, nil, slog.New(slog.DiscardHandler))
	s.refresh(t.Context())
	for addr, want := range map[string]string{"2001:db8::1": "router-rest", "::ffff:192.0.2.2": "ap"} {
		if dev, ok := s.attribute(netip.MustParseAddr(addr)); !ok || dev != want {
			t.Errorf("attribute(%s) = %q %v, want %q", addr, dev, ok, want)
		}
	}
}
