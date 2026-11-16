package presence

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/endreszabo/mikrotik-presence-sensor/internal/mikrotik"
)

type fakeTimer struct {
	c       *fakeClock
	at      time.Time
	f       func()
	stopped bool
}

func (t *fakeTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := !t.stopped
	t.stopped = true
	return was
}

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{c: c, at: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	return t
}

// Advance moves time forward, firing due timers in order.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	end := c.now.Add(d)
	c.mu.Unlock()
	for {
		c.mu.Lock()
		var next *fakeTimer
		for _, t := range c.timers {
			if !t.stopped && !t.at.After(end) && (next == nil || t.at.Before(next.at)) {
				next = t
			}
		}
		if next == nil {
			c.now = end
			c.mu.Unlock()
			return
		}
		next.stopped = true
		c.now = next.at
		c.mu.Unlock()
		next.f()
	}
}

const (
	aliceMAC  = "aa:bb:cc:dd:ee:01"
	alice2MAC = "aa:bb:cc:dd:ee:02"
	bobMAC    = "aa:bb:cc:dd:ee:03"
)

type harness struct {
	t     *testing.T
	clock *fakeClock
	e     *Engine
	mu    sync.Mutex
	got   []State
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, clock: newFakeClock()}
	h.e = New(Config{
		ConsiderHome:   3 * time.Minute,
		EventTTL:       2 * time.Minute,
		Sources:        mikrotik.AllKinds(),
		ARPStates:      []string{"reachable", "delay", "probe"},
		NeighborStates: []string{"reachable"},
	}, []Person{
		{ID: "alice", Name: "Alice", MACs: []string{aliceMAC, alice2MAC}},
		{ID: "bob", Name: "Bob", MACs: []string{bobMAC}},
	}, h.clock, nil)
	h.e.OnChange(func(s State) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.got = append(h.got, s)
	})
	h.e.Start()
	return h
}

func (h *harness) status(id string) string {
	h.t.Helper()
	for _, s := range h.e.States() {
		if s.ID == id {
			return s.Status
		}
	}
	h.t.Fatalf("no person %q", id)
	return ""
}

func (h *harness) published(id string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, s := range h.got {
		if s.ID == id {
			out = append(out, s.Status)
		}
	}
	return out
}

func wifi(mac, device string) mikrotik.Observation {
	sig := -55
	return mikrotik.Observation{MAC: mac, Kind: mikrotik.KindWifi, Device: device, Interface: "wifi1", SSID: "home", Signal: &sig}
}

func TestStartupUnknownThenNotHome(t *testing.T) {
	h := newHarness(t)
	if s := h.status("alice"); s != StatusUnknown {
		t.Fatalf("initial status = %s", s)
	}
	h.clock.Advance(2 * time.Minute)
	if len(h.published("alice")) != 0 {
		t.Fatal("published before startup grace elapsed")
	}
	h.clock.Advance(time.Minute)
	if s := h.status("alice"); s != StatusNotHome {
		t.Fatalf("status after grace = %s", s)
	}
	if p := h.published("alice"); !slices.Equal(p, []string{StatusNotHome}) {
		t.Fatalf("published = %v", p)
	}
}

func TestSnapshotArrivalAndDeparture(t *testing.T) {
	h := newHarness(t)
	h.e.UpdateSnapshot("ap1", mikrotik.KindWifi, []mikrotik.Observation{wifi(aliceMAC, "ap1"), wifi("00:00:5e:00:53:99", "ap1")})
	if s := h.status("alice"); s != StatusHome {
		t.Fatalf("status = %s", s)
	}
	st := h.e.States()[0]
	if st.Attributes.SSID != "home" || st.Attributes.Device != "ap1" || *st.Attributes.Signal != -55 {
		t.Errorf("attributes = %+v", st.Attributes)
	}
	// Phone leaves: next poll no longer lists it.
	h.clock.Advance(30 * time.Second)
	h.e.UpdateSnapshot("ap1", mikrotik.KindWifi, nil)
	h.clock.Advance(2 * time.Minute)
	if s := h.status("alice"); s != StatusHome {
		t.Fatalf("left before consider_home elapsed: %s", s)
	}
	h.clock.Advance(time.Minute)
	if s := h.status("alice"); s != StatusNotHome {
		t.Fatalf("status = %s", s)
	}
	if p := h.published("alice"); !slices.Equal(p, []string{StatusHome, StatusNotHome}) {
		t.Fatalf("published = %v", p)
	}
}

func TestRoamingDoesNotFlap(t *testing.T) {
	h := newHarness(t)
	h.e.UpdateSnapshot("ap1", mikrotik.KindWifi, []mikrotik.Observation{wifi(aliceMAC, "ap1")})
	h.e.HandleEvent(Event{Type: EventLeave, MAC: aliceMAC, Kind: mikrotik.KindWifi, Device: "ap1"})
	h.clock.Advance(5 * time.Second)
	h.e.HandleEvent(Event{Type: EventJoin, MAC: aliceMAC, Kind: mikrotik.KindWifi, Device: "ap2", Interface: "wifi2"})
	h.clock.Advance(10 * time.Minute)
	if s := h.status("alice"); s != StatusHome {
		t.Fatalf("status = %s", s)
	}
	p := h.published("alice")
	if slices.Contains(p, StatusNotHome) {
		t.Fatalf("flapped: %v", p)
	}
	if st := h.e.States()[0]; st.Attributes.Device != "ap2" || st.Attributes.Interface != "wifi2" {
		t.Errorf("attributes after roam = %+v", st.Attributes)
	}
}

func TestJoinEventSupersededBySnapshot(t *testing.T) {
	h := newHarness(t)
	h.e.HandleEvent(Event{Type: EventJoin, MAC: bobMAC, Kind: mikrotik.KindWireless, Device: "ap1"})
	if s := h.status("bob"); s != StatusHome {
		t.Fatalf("status = %s", s)
	}
	// The disconnect log line was lost, but a later poll shows bob gone.
	h.clock.Advance(time.Second)
	h.e.UpdateSnapshot("ap1", mikrotik.KindWireless, nil)
	h.clock.Advance(3 * time.Minute)
	if s := h.status("bob"); s != StatusNotHome {
		t.Fatalf("status = %s", s)
	}
}

func TestSeenEventExpires(t *testing.T) {
	h := newHarness(t)
	h.e.HandleEvent(Event{Type: EventSeen, MAC: bobMAC, Kind: mikrotik.KindDHCP, Device: "router", IP: "192.0.2.5"})
	if s := h.status("bob"); s != StatusHome {
		t.Fatalf("status = %s", s)
	}
	h.clock.Advance(2*time.Minute + time.Second) // EventTTL
	if s := h.status("bob"); s != StatusHome {
		t.Fatalf("away before consider_home: %s", s)
	}
	h.clock.Advance(3 * time.Minute)
	if s := h.status("bob"); s != StatusNotHome {
		t.Fatalf("status = %s", s)
	}
}

func TestStateFilteringAndIPOrder(t *testing.T) {
	h := newHarness(t)
	h.e.UpdateSnapshot("router", mikrotik.KindARP, []mikrotik.Observation{
		{MAC: aliceMAC, Kind: mikrotik.KindARP, Device: "router", IP: "192.0.2.10", State: "stale"},
	})
	h.e.UpdateSnapshot("router", mikrotik.KindNeighbor, []mikrotik.Observation{
		{MAC: aliceMAC, Kind: mikrotik.KindNeighbor, Device: "router", IP: "2001:db8::10", State: "stale"},
	})
	if s := h.status("alice"); s != StatusUnknown {
		t.Fatalf("stale entries counted: %s", s)
	}
	h.e.UpdateSnapshot("router", mikrotik.KindNeighbor, []mikrotik.Observation{
		{MAC: alice2MAC, Kind: mikrotik.KindNeighbor, Device: "router", IP: "fe80::1", State: "reachable"},
		{MAC: aliceMAC, Kind: mikrotik.KindNeighbor, Device: "router", IP: "2001:db8::10", State: "reachable"},
	})
	h.e.UpdateSnapshot("router", mikrotik.KindARP, []mikrotik.Observation{
		{MAC: aliceMAC, Kind: mikrotik.KindARP, Device: "router", IP: "192.0.2.10", State: "reachable"},
	})
	st := h.e.States()[0]
	if st.Status != StatusHome {
		t.Fatalf("status = %s", st.Status)
	}
	if want := []string{"2001:db8::10", "fe80::1", "192.0.2.10"}; !slices.Equal(st.Attributes.IPAddresses, want) {
		t.Errorf("ips = %v, want %v", st.Attributes.IPAddresses, want)
	}
	if want := []string{"arp", "neighbor"}; !slices.Equal(st.Attributes.Sources, want) {
		t.Errorf("sources = %v", st.Attributes.Sources)
	}
}

func TestInvalidateSnapshot(t *testing.T) {
	h := newHarness(t)
	h.e.UpdateSnapshot("sw1", mikrotik.KindBridgeHost, []mikrotik.Observation{{MAC: bobMAC, Kind: mikrotik.KindBridgeHost, Device: "sw1"}})
	h.e.InvalidateSnapshot("sw1", mikrotik.KindBridgeHost)
	h.clock.Advance(3 * time.Minute)
	if s := h.status("bob"); s != StatusNotHome {
		t.Fatalf("status = %s", s)
	}
}

func TestDisabledSourceAndUnknownMAC(t *testing.T) {
	h := newHarness(t)
	h.e.sources[mikrotik.KindBridgeHost] = false
	h.e.UpdateSnapshot("sw1", mikrotik.KindBridgeHost, []mikrotik.Observation{{MAC: bobMAC, Kind: mikrotik.KindBridgeHost, Device: "sw1"}})
	if s := h.status("bob"); s != StatusUnknown {
		t.Fatalf("disabled source counted: %s", s)
	}
	if h.e.HandleEvent(Event{Type: EventJoin, MAC: "00:00:5e:00:53:01", Device: "ap1"}) {
		t.Fatal("untracked MAC reported as tracked")
	}
}

func TestSignalChangeIsNotSignificant(t *testing.T) {
	h := newHarness(t)
	h.e.UpdateSnapshot("ap1", mikrotik.KindWifi, []mikrotik.Observation{wifi(aliceMAC, "ap1")})
	o := wifi(aliceMAC, "ap1")
	sig := -70
	o.Signal = &sig
	h.clock.Advance(30 * time.Second)
	h.e.UpdateSnapshot("ap1", mikrotik.KindWifi, []mikrotik.Observation{o})
	if n := len(h.published("alice")); n != 1 {
		t.Fatalf("published %d times, want 1", n)
	}
}
