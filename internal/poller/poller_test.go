package poller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/endreszabo/mikrotik-presence-sensor/internal/mikrotik"
)

type fakeClient struct {
	mu     sync.Mutex
	calls  [][]mikrotik.Kind
	result func(call int, kinds []mikrotik.Kind) mikrotik.Result
}

func (f *fakeClient) Poll(_ context.Context, kinds []mikrotik.Kind) mikrotik.Result {
	f.mu.Lock()
	f.calls = append(f.calls, slices.Clone(kinds))
	n := len(f.calls)
	f.mu.Unlock()
	return f.result(n, kinds)
}
func (f *fakeClient) Kinds() []mikrotik.Kind {
	return []mikrotik.Kind{mikrotik.KindWifi, mikrotik.KindWireless}
}
func (f *fakeClient) Close() error { return nil }

func (f *fakeClient) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type fakeSink struct {
	mu          sync.Mutex
	updates     map[mikrotik.Kind]int
	invalidated []mikrotik.Kind
}

func (s *fakeSink) UpdateSnapshot(_ string, k mikrotik.Kind, _ []mikrotik.Observation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updates[k]++
}

func (s *fakeSink) InvalidateSnapshot(_ string, k mikrotik.Kind) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalidated = append(s.invalidated, k)
}

func newPoller(c *fakeClient, kinds []mikrotik.Kind) (*Poller, *fakeSink) {
	sink := &fakeSink{updates: map[mikrotik.Kind]int{}}
	p := New(Options{Device: "ap1", Transport: "rest", Kinds: kinds, Interval: time.Hour, StaleAfter: 2},
		c, sink, slog.New(slog.DiscardHandler))
	return p, sink
}

func TestAutoKindsDropUnsupported(t *testing.T) {
	c := &fakeClient{result: func(_ int, kinds []mikrotik.Kind) mikrotik.Result {
		r := mikrotik.NewResult()
		for _, k := range kinds {
			if k == mikrotik.KindWireless {
				r.Errors[k] = fmt.Errorf("x: %w", mikrotik.ErrUnsupported)
			} else {
				r.Snapshots[k] = nil
			}
		}
		return r
	}}
	p, sink := newPoller(c, nil)
	var statuses []Status
	p.OnStatus(func(s Status) { statuses = append(statuses, s) })
	p.pollOnce(t.Context())
	p.pollOnce(t.Context())
	if got := c.calls[1]; !slices.Equal(got, []mikrotik.Kind{mikrotik.KindWifi}) {
		t.Errorf("second poll kinds = %v", got)
	}
	if sink.updates[mikrotik.KindWifi] != 2 {
		t.Errorf("updates = %v", sink.updates)
	}
	if st := p.Status(); !st.OK || !slices.Equal(st.Kinds, []string{"wifi"}) {
		t.Errorf("status = %+v", st)
	}
	if len(statuses) != 2 {
		t.Errorf("status callbacks = %d", len(statuses))
	}
}

func TestFailuresInvalidateAndBackoff(t *testing.T) {
	c := &fakeClient{result: func(_ int, kinds []mikrotik.Kind) mikrotik.Result {
		r := mikrotik.NewResult()
		for _, k := range kinds {
			r.Errors[k] = errors.New("timeout")
		}
		return r
	}}
	p, sink := newPoller(c, []mikrotik.Kind{mikrotik.KindARP})
	if wait := p.pollOnce(t.Context()); wait != 2*time.Second {
		t.Errorf("first backoff = %v", wait)
	}
	if len(sink.invalidated) != 0 {
		t.Error("invalidated too early")
	}
	if wait := p.pollOnce(t.Context()); wait != 4*time.Second {
		t.Errorf("second backoff = %v", wait)
	}
	if !slices.Equal(sink.invalidated, []mikrotik.Kind{mikrotik.KindARP}) {
		t.Errorf("invalidated = %v", sink.invalidated)
	}
	st := p.Status()
	if st.OK || st.ConsecutiveFailures != 2 || st.LastError == "" {
		t.Errorf("status = %+v", st)
	}
	// Explicit kinds are never dropped for plain errors.
	if !slices.Equal(st.Kinds, []string{"arp"}) {
		t.Errorf("kinds = %v", st.Kinds)
	}
}

func TestTriggerPollsEarly(t *testing.T) {
	c := &fakeClient{result: func(int, []mikrotik.Kind) mikrotik.Result {
		r := mikrotik.NewResult()
		r.Snapshots[mikrotik.KindWifi] = nil
		return r
	}}
	p, _ := newPoller(c, []mikrotik.Kind{mikrotik.KindWifi})
	p.maxJitter = time.Millisecond
	p.opts.Debounce = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = p.Run(ctx) }()
	waitFor(t, func() bool { return c.callCount() == 1 })
	before := c.callCount()
	p.Trigger()
	p.Trigger() // coalesced
	waitFor(t, func() bool { return c.callCount() == before+1 })
	time.Sleep(50 * time.Millisecond)
	if n := c.callCount(); n != before+1 {
		t.Errorf("polls after trigger = %d, want %d", n, before+1)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(time.Millisecond)
	}
}
