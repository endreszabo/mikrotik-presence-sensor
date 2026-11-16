// Package presence turns MAC address sightings (polled table snapshots and
// live log events) into per-person home/not_home states.
//
// A person is home while at least one of their MACs has a current sighting.
// When the last sighting disappears an away timer of ConsiderHome starts;
// only when it fires does the person become not_home. This absorbs roaming
// between access points (disconnect from one, connect to the next), short
// polling gaps and phones dozing off.
package presence

import (
	"context"
	"log/slog"
	"maps"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/endreszabo/mikrotik-presence-sensor/internal/mikrotik"
)

// Clock abstracts time for tests.
type Clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) Timer
}

// Timer is the subset of *time.Timer the engine uses.
type Timer interface{ Stop() bool }

type realClock struct{}

func (realClock) Now() time.Time                            { return time.Now() }
func (realClock) AfterFunc(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }

// RealClock is the wall clock.
var RealClock Clock = realClock{}

// Config holds the presence tuning knobs.
type Config struct {
	ConsiderHome   time.Duration
	EventTTL       time.Duration
	Sources        []mikrotik.Kind
	ARPStates      []string
	NeighborStates []string
}

// Person is a tracked person and the MACs of their devices.
type Person struct {
	ID   string
	Name string
	Icon string
	MACs []string
}

// EventType is the meaning of a live log event.
type EventType string

const (
	EventJoin  EventType = "join"  // associated; valid until a leave or a newer snapshot without it
	EventLeave EventType = "leave" // disassociated
	EventSeen  EventType = "seen"  // a one-off sign of life, valid for EventTTL
)

// Event is a sighting reported by a log message.
type Event struct {
	Type      EventType
	MAC       string
	Kind      mikrotik.Kind // may be empty for custom rules
	Device    string
	Interface string
	IP        string
}

// Attributes are the details published alongside a person's state.
type Attributes struct {
	MAC         string     `json:"mac,omitempty"`
	Device      string     `json:"device,omitempty"`
	Interface   string     `json:"interface,omitempty"`
	SSID        string     `json:"ssid,omitempty"`
	Band        string     `json:"band,omitempty"`
	Signal      *int       `json:"signal,omitempty"`
	IPAddresses []string   `json:"ip_addresses,omitempty"`
	Sources     []string   `json:"sources,omitempty"`
	SeenBy      []string   `json:"seen_by,omitempty"`
	LastSeen    *time.Time `json:"last_seen,omitempty"`
}

// same reports whether two attribute sets differ only in fast-moving values
// (signal, last seen) that should not trigger a publish on their own.
func (a Attributes) same(b Attributes) bool {
	return a.MAC == b.MAC && a.Device == b.Device && a.Interface == b.Interface &&
		a.SSID == b.SSID && a.Band == b.Band &&
		slices.Equal(a.IPAddresses, b.IPAddresses) && slices.Equal(a.Sources, b.Sources) &&
		slices.Equal(a.SeenBy, b.SeenBy)
}

// Status values.
const (
	StatusHome    = "home"
	StatusNotHome = "not_home"
	StatusUnknown = "unknown"
)

// State is a person's current presence.
type State struct {
	ID           string                 `json:"id"`
	Name         string                 `json:"name"`
	Icon         string                 `json:"icon,omitempty"`
	Status       string                 `json:"state"`
	Since        time.Time              `json:"since,omitzero"`
	Attributes   Attributes             `json:"attributes"`
	Observations []mikrotik.Observation `json:"observations,omitempty"`
}

// Home reports whether the person is home.
func (s State) Home() bool { return s.Status == StatusHome }

type snapKey struct {
	device string
	kind   mikrotik.Kind
}

type eventKey struct {
	mac    string
	device string
	kind   mikrotik.Kind
}

type snapshot struct {
	at  time.Time
	obs map[string][]mikrotik.Observation // by MAC
}

type personState struct {
	p           Person
	status      string
	since       time.Time
	attrs       Attributes
	obs         []mikrotik.Observation
	awayTimer   Timer
	awayGen     int // guards against a stopped timer whose callback already started
	expiryTimer Timer
	expiryAt    time.Time
}

// Engine holds all sightings and person states. It is safe for concurrent use.
type Engine struct {
	cfg     Config
	clock   Clock
	log     *slog.Logger
	sources map[mikrotik.Kind]bool
	arpOK   map[string]bool
	nbOK    map[string]bool

	mu        sync.Mutex
	people    []*personState
	byMAC     map[string]*personState
	snapshots map[snapKey]snapshot
	events    map[eventKey]mikrotik.Observation
	listeners []func(State)
}

// New creates an engine. Call Start to arm the initial timers.
func New(cfg Config, people []Person, clock Clock, log *slog.Logger) *Engine {
	if clock == nil {
		clock = RealClock
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if cfg.EventTTL == 0 {
		cfg.EventTTL = cfg.ConsiderHome
	}
	set := func(s []string) map[string]bool {
		m := map[string]bool{}
		for _, v := range s {
			m[v] = true
		}
		return m
	}
	e := &Engine{
		cfg:       cfg,
		clock:     clock,
		log:       log,
		sources:   map[mikrotik.Kind]bool{},
		arpOK:     set(cfg.ARPStates),
		nbOK:      set(cfg.NeighborStates),
		byMAC:     map[string]*personState{},
		snapshots: map[snapKey]snapshot{},
		events:    map[eventKey]mikrotik.Observation{},
	}
	for _, k := range cfg.Sources {
		e.sources[k] = true
	}
	for _, p := range people {
		ps := &personState{p: p, status: StatusUnknown}
		e.people = append(e.people, ps)
		for _, m := range p.MACs {
			e.byMAC[m] = ps
		}
	}
	return e
}

// OnChange registers a callback invoked (outside the engine lock) whenever a
// person's state or significant attributes change.
func (e *Engine) OnChange(f func(State)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.listeners = append(e.listeners, f)
}

// Start evaluates everyone once. People not seen within ConsiderHome of
// startup become not_home; until then they are "unknown" and not published,
// so a restart does not fire spurious "left home" automations.
func (e *Engine) Start() {
	e.update(func() {})
}

// Tracks reports whether the MAC belongs to a configured person.
func (e *Engine) Tracks(mac string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.byMAC[mac]
	return ok
}

// UpdateSnapshot replaces the table of the given kind for a device with a
// fresh poll result.
func (e *Engine) UpdateSnapshot(device string, kind mikrotik.Kind, obs []mikrotik.Observation) {
	e.update(func() {
		now := e.clock.Now()
		s := snapshot{at: now, obs: map[string][]mikrotik.Observation{}}
		for _, o := range obs {
			if _, ok := e.byMAC[o.MAC]; ok { // only keep what matters
				s.obs[o.MAC] = append(s.obs[o.MAC], o)
			}
		}
		e.snapshots[snapKey{device, kind}] = s
		// A fresh table is authoritative for join events it does not
		// contain; this covers missed "disconnected" log lines.
		for k, ev := range e.events {
			if k.device == device && k.kind == kind && ev.State == string(EventJoin) &&
				ev.SeenAt.Before(now) && s.obs[k.mac] == nil {
				delete(e.events, k)
			}
		}
	})
}

// InvalidateSnapshot forgets a device table, e.g. after repeated poll failures.
func (e *Engine) InvalidateSnapshot(device string, kind mikrotik.Kind) {
	e.update(func() { delete(e.snapshots, snapKey{device, kind}) })
}

// HandleEvent applies a live log event. It returns false when the MAC does
// not belong to anyone.
func (e *Engine) HandleEvent(ev Event) bool {
	tracked := false
	e.update(func() {
		if _, tracked = e.byMAC[ev.MAC]; !tracked {
			return
		}
		key := eventKey{ev.MAC, ev.Device, ev.Kind}
		switch ev.Type {
		case EventJoin, EventSeen:
			e.events[key] = mikrotik.Observation{
				MAC: ev.MAC, Kind: ev.Kind, Device: ev.Device, Interface: ev.Interface,
				IP: ev.IP, State: string(ev.Type), SeenAt: e.clock.Now(),
			}
		case EventLeave:
			delete(e.events, key)
			// The MAC is gone from that device right now, whatever the last
			// poll said. Log rules cannot always tell the wifi and wireless
			// packages apart, so drop it from every association table.
			for sk, s := range e.snapshots {
				if sk.device != ev.Device || s.obs[ev.MAC] == nil {
					continue
				}
				if sk.kind == ev.Kind || (sk.kind.Wireless() && (ev.Kind == "" || ev.Kind.Wireless())) {
					delete(s.obs, ev.MAC)
				}
			}
			for k := range e.events {
				if k.mac == ev.MAC && k.device == ev.Device && (k.kind == ev.Kind || k.kind.Wireless() && ev.Kind.Wireless()) {
					delete(e.events, k)
				}
			}
		}
	})
	return tracked
}

// States returns every person's current state, including observations.
func (e *Engine) States() []State {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]State, 0, len(e.people))
	for _, ps := range e.people {
		out = append(out, ps.state())
	}
	return out
}

// update runs fn under the lock, re-evaluates everyone and notifies
// listeners of changes after unlocking.
func (e *Engine) update(fn func()) {
	e.mu.Lock()
	fn()
	var changed []State
	for _, ps := range e.people {
		if e.evaluate(ps) {
			changed = append(changed, ps.state())
		}
	}
	listeners := slices.Clone(e.listeners)
	e.mu.Unlock()
	e.notify(listeners, changed)
}

func (e *Engine) notify(listeners []func(State), changed []State) {
	for _, s := range changed {
		for _, f := range listeners {
			f(s)
		}
	}
}

func (ps *personState) state() State {
	return State{
		ID: ps.p.ID, Name: ps.p.Name, Icon: ps.p.Icon, Status: ps.status, Since: ps.since,
		Attributes: ps.attrs, Observations: slices.Clone(ps.obs),
	}
}

// current returns the person's valid sightings and the earliest time one of
// them expires on its own (zero if none do).
func (e *Engine) current(ps *personState, now time.Time) (obs []mikrotik.Observation, expiry time.Time) {
	noteExpiry := func(t time.Time) {
		if expiry.IsZero() || t.Before(expiry) {
			expiry = t
		}
	}
	for _, mac := range ps.p.MACs {
		for sk, s := range e.snapshots {
			if !e.sources[sk.kind] {
				continue
			}
			for _, o := range s.obs[mac] {
				switch o.Kind {
				case mikrotik.KindARP:
					if !e.arpOK[o.State] {
						continue
					}
				case mikrotik.KindNeighbor:
					if !e.nbOK[o.State] {
						continue
					}
				case mikrotik.KindDHCP:
					exp := o.SeenAt.Add(e.cfg.EventTTL)
					if !now.Before(exp) {
						continue
					}
					noteExpiry(exp)
				}
				obs = append(obs, o)
			}
		}
	}
	for k, o := range e.events {
		if e.byMAC[k.mac] != ps || (k.kind != "" && !e.sources[k.kind]) {
			continue
		}
		if o.State == string(EventSeen) {
			exp := o.SeenAt.Add(e.cfg.EventTTL)
			if !now.Before(exp) {
				delete(e.events, k)
				continue
			}
			noteExpiry(exp)
		}
		obs = append(obs, o)
	}
	slices.SortFunc(obs, func(a, b mikrotik.Observation) int {
		if a.Kind.Rank() != b.Kind.Rank() {
			return a.Kind.Rank() - b.Kind.Rank()
		}
		return b.SeenAt.Compare(a.SeenAt)
	})
	return obs, expiry
}

// evaluate recomputes a person's state; it reports whether listeners must be
// told. Must be called with e.mu held.
func (e *Engine) evaluate(ps *personState) bool {
	now := e.clock.Now()
	obs, expiry := e.current(ps, now)
	ps.obs = obs
	e.armExpiry(ps, expiry)

	if len(obs) > 0 {
		if ps.awayTimer != nil {
			ps.awayTimer.Stop()
			ps.awayTimer = nil
		}
		attrs := buildAttributes(obs)
		if ps.status != StatusHome {
			e.log.Info("person arrived", "person", ps.p.ID, "via", attrs.Sources, "device", attrs.Device)
			ps.status, ps.since, ps.attrs = StatusHome, now, attrs
			return true
		}
		significant := !ps.attrs.same(attrs)
		ps.attrs = attrs
		return significant
	}

	if ps.status != StatusNotHome && ps.awayTimer == nil {
		e.log.Debug("no current sighting, starting away timer", "person", ps.p.ID, "consider_home", e.cfg.ConsiderHome)
		ps.awayGen++
		gen := ps.awayGen
		ps.awayTimer = e.clock.AfterFunc(e.cfg.ConsiderHome, func() { e.awayTimerFired(ps, gen) })
	}
	return false
}

func (e *Engine) awayTimerFired(ps *personState, gen int) {
	e.mu.Lock()
	if gen != ps.awayGen || ps.awayTimer == nil {
		e.mu.Unlock()
		return
	}
	ps.awayTimer = nil
	var changed []State
	if obs, _ := e.current(ps, e.clock.Now()); len(obs) == 0 && ps.status != StatusNotHome {
		e.log.Info("person left", "person", ps.p.ID, "previous", ps.status)
		ps.status, ps.since, ps.obs = StatusNotHome, e.clock.Now(), nil
		ps.attrs = Attributes{LastSeen: ps.attrs.LastSeen}
		changed = append(changed, ps.state())
	}
	listeners := slices.Clone(e.listeners)
	e.mu.Unlock()
	e.notify(listeners, changed)
	// Re-arm or settle anything that changed while the timer was running.
	e.update(func() {})
}

func (e *Engine) armExpiry(ps *personState, at time.Time) {
	if at.Equal(ps.expiryAt) {
		return
	}
	if ps.expiryTimer != nil {
		ps.expiryTimer.Stop()
		ps.expiryTimer = nil
	}
	ps.expiryAt = at
	if at.IsZero() {
		return
	}
	ps.expiryTimer = e.clock.AfterFunc(at.Sub(e.clock.Now()), func() {
		e.update(func() {
			ps.expiryTimer, ps.expiryAt = nil, time.Time{}
		})
	})
}

func buildAttributes(obs []mikrotik.Observation) Attributes {
	var a Attributes
	best := obs[0]
	a.MAC, a.Device, a.Interface = best.MAC, best.Device, best.Interface
	if best.Kind.Wireless() || best.SSID != "" {
		a.SSID, a.Band, a.Signal = best.SSID, best.Band, best.Signal
	}
	ips, sources, seenBy := map[string]bool{}, map[string]bool{}, map[string]bool{}
	var last time.Time
	for _, o := range obs {
		if o.IP != "" {
			ips[o.IP] = true
		}
		kind := string(o.Kind)
		if kind == "" {
			kind = "log"
		}
		sources[kind] = true
		seenBy[o.Device] = true
		if o.SeenAt.After(last) {
			last = o.SeenAt
		}
	}
	a.IPAddresses = sortIPs(slices.Collect(maps.Keys(ips)))
	a.Sources = slices.Sorted(maps.Keys(sources))
	a.SeenBy = slices.Sorted(maps.Keys(seenBy))
	if !last.IsZero() {
		a.LastSeen = &last
	}
	return a
}

// sortIPs orders IPv6 global addresses first, then IPv6 link-local, then IPv4.
func sortIPs(ips []string) []string {
	class := func(s string) int {
		a, err := netip.ParseAddr(s)
		switch {
		case err != nil:
			return 3
		case a.Is4() || a.Is4In6():
			return 2
		case a.IsLinkLocalUnicast():
			return 1
		default:
			return 0
		}
	}
	slices.SortFunc(ips, func(a, b string) int {
		if ca, cb := class(a), class(b); ca != cb {
			return ca - cb
		}
		pa, _ := netip.ParseAddr(a)
		pb, _ := netip.ParseAddr(b)
		return pa.Compare(pb)
	})
	return ips
}

// Run blocks until ctx is done and then stops all timers.
func (e *Engine) Run(ctx context.Context) error {
	e.Start()
	<-ctx.Done()
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, ps := range e.people {
		for _, t := range []Timer{ps.awayTimer, ps.expiryTimer} {
			if t != nil {
				t.Stop()
			}
		}
	}
	return nil
}
