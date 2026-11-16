// Package poller periodically polls one MikroTik device and feeds the
// resulting table snapshots to the presence engine.
package poller

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"github.com/endreszabo/zorplogger/session"

	"github.com/endreszabo/mikrotik-presence-sensor/internal/mikrotik"
)

// Sink receives poll results.
type Sink interface {
	UpdateSnapshot(device string, kind mikrotik.Kind, obs []mikrotik.Observation)
	InvalidateSnapshot(device string, kind mikrotik.Kind)
}

// Options configures a Poller.
type Options struct {
	Device    string
	Transport string
	// Kinds to poll; nil means everything the client supports.
	Kinds []mikrotik.Kind
	// Probe quietly drops kinds the device reports as unsupported (e.g. a
	// package that is not installed). Implied when Kinds is nil.
	Probe    bool
	Interval time.Duration
	// StaleAfter consecutive failures of a kind invalidate its snapshot.
	StaleAfter int
	// Debounce delays triggered re-polls so bursts of log events coalesce.
	Debounce time.Duration
}

// Status is a point-in-time view of a poller for health and diagnostics.
type Status struct {
	Device              string    `json:"device"`
	Transport           string    `json:"transport"`
	Kinds               []string  `json:"kinds"`
	OK                  bool      `json:"ok"`
	LastAttempt         time.Time `json:"last_attempt,omitzero"`
	LastSuccess         time.Time `json:"last_success,omitzero"`
	LastError           string    `json:"last_error,omitempty"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
}

// Poller polls one device.
type Poller struct {
	opts    Options
	client  mikrotik.Client
	sink    Sink
	log     *slog.Logger
	trigger chan struct{}
	auto    bool
	// maxJitter bounds the random delay before the first poll.
	maxJitter time.Duration

	mu       sync.Mutex
	kinds    []mikrotik.Kind
	failures map[mikrotik.Kind]int
	status   Status
	onStatus []func(Status)
	onReport []func(Report)
}

// Report describes one finished poll cycle, for metrics.
type Report struct {
	Status  Status
	Elapsed time.Duration
	Kinds   []KindReport
}

// KindReport is the outcome for one table within a poll cycle. Kinds found
// to be unsupported are not reported.
type KindReport struct {
	Kind     mikrotik.Kind
	Duration time.Duration
	// Items is the number of table entries (all MACs, not only tracked ones).
	Items int
	Err   error
}

// New creates a poller. Kinds the transport cannot provide are dropped.
func New(opts Options, client mikrotik.Client, sink Sink, log *slog.Logger) *Poller {
	if opts.StaleAfter <= 0 {
		opts.StaleAfter = 3
	}
	if opts.Debounce <= 0 {
		opts.Debounce = 2 * time.Second
	}
	p := &Poller{
		opts:      opts,
		client:    client,
		sink:      sink,
		log:       log.With("device", opts.Device),
		trigger:   make(chan struct{}, 1),
		auto:      opts.Probe || opts.Kinds == nil,
		maxJitter: min(opts.Interval, 5*time.Second),
		failures:  map[mikrotik.Kind]int{},
	}
	if opts.Kinds == nil {
		p.kinds = client.Kinds()
	} else {
		p.kinds = slices.Clone(opts.Kinds)
	}
	p.status = Status{Device: opts.Device, Transport: opts.Transport, Kinds: kindNames(p.kinds)}
	return p
}

// OnStatus registers a callback invoked after every poll.
func (p *Poller) OnStatus(f func(Status)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.onStatus = append(p.onStatus, f)
}

// OnReport registers a callback invoked after every poll with its details.
func (p *Poller) OnReport(f func(Report)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.onReport = append(p.onReport, f)
}

// Device returns the device name.
func (p *Poller) Device() string { return p.opts.Device }

// Trigger requests an early poll (debounced). It never blocks.
func (p *Poller) Trigger() {
	select {
	case p.trigger <- struct{}{}:
	default:
	}
}

// Status returns the current status.
func (p *Poller) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.status
	s.Kinds = slices.Clone(s.Kinds)
	return s
}

// Run polls until ctx is cancelled.
func (p *Poller) Run(ctx context.Context) error {
	defer p.client.Close()
	// Spread the first polls of many devices over a few seconds.
	jitter := time.Duration(rand.Int64N(int64(p.maxJitter) + 1))
	timer := time.NewTimer(jitter)
	defer timer.Stop()
	var nextDue time.Time

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-p.trigger:
			// Pull the next poll forward unless it is already imminent.
			if nextDue.IsZero() || time.Until(nextDue) > p.opts.Debounce {
				timer.Reset(p.opts.Debounce)
				nextDue = time.Now().Add(p.opts.Debounce)
				p.log.Debug("re-poll triggered by log event")
			}
		case <-timer.C:
			wait := p.pollOnce(ctx)
			if ctx.Err() != nil {
				return nil
			}
			timer.Reset(wait)
			nextDue = time.Now().Add(wait)
		}
	}
}

// pollOnce runs one cycle and returns the delay until the next one.
func (p *Poller) pollOnce(ctx context.Context) time.Duration {
	ctx = session.NewContext(ctx)
	p.mu.Lock()
	kinds := slices.Clone(p.kinds)
	p.mu.Unlock()
	if len(kinds) == 0 {
		return p.opts.Interval
	}

	start := time.Now()
	res := p.client.Poll(ctx, kinds)
	elapsed := time.Since(start)

	var (
		errs        []error
		unsupported []mikrotik.Kind
		stale       []mikrotik.Kind
		counts      = map[string]int{}
	)
	p.mu.Lock()
	for _, k := range kinds {
		if obs, ok := res.Snapshots[k]; ok {
			p.failures[k] = 0
			counts[string(k)] = len(obs)
			continue
		}
		err := res.Errors[k]
		if err == nil {
			continue
		}
		if errors.Is(err, mikrotik.ErrUnsupported) {
			unsupported = append(unsupported, k)
			continue
		}
		errs = append(errs, err)
		p.failures[k]++
		if p.failures[k] == p.opts.StaleAfter {
			p.log.WarnContext(ctx, "dropping stale snapshot after repeated failures", "kind", k, "failures", p.failures[k])
			stale = append(stale, k)
		}
	}
	if len(unsupported) > 0 {
		p.kinds = slices.DeleteFunc(p.kinds, func(k mikrotik.Kind) bool { return slices.Contains(unsupported, k) })
		p.status.Kinds = kindNames(p.kinds)
		level := slog.LevelInfo
		if !p.auto {
			level = slog.LevelWarn
		}
		p.log.Log(ctx, level, "device does not provide some tables, no longer polling them",
			"kinds", kindNames(unsupported), "remaining", p.status.Kinds)
	}

	now := time.Now()
	p.status.LastAttempt = now
	failed := len(errs) > 0
	if failed {
		p.status.OK = false
		p.status.ConsecutiveFailures++
		p.status.LastError = errors.Join(errs...).Error()
	} else {
		p.status.OK = true
		p.status.ConsecutiveFailures = 0
		p.status.LastError = ""
		p.status.LastSuccess = now
	}
	status := p.status
	status.Kinds = slices.Clone(status.Kinds)
	failures := p.status.ConsecutiveFailures
	listeners := slices.Clone(p.onStatus)
	reporters := slices.Clone(p.onReport)
	p.mu.Unlock()

	// Feed the engine outside our lock: its listeners may call Status().
	for _, k := range kinds {
		if obs, ok := res.Snapshots[k]; ok {
			p.sink.UpdateSnapshot(p.opts.Device, k, obs)
		}
	}
	for _, k := range stale {
		p.sink.InvalidateSnapshot(p.opts.Device, k)
	}
	for _, f := range listeners {
		f(status)
	}
	if len(reporters) > 0 {
		rep := Report{Status: status, Elapsed: elapsed}
		for _, k := range kinds {
			if slices.Contains(unsupported, k) {
				continue
			}
			obs, ok := res.Snapshots[k]
			if !ok && res.Errors[k] == nil {
				continue
			}
			rep.Kinds = append(rep.Kinds, KindReport{Kind: k, Duration: res.Durations[k], Items: len(obs), Err: res.Errors[k]})
		}
		for _, f := range reporters {
			f(rep)
		}
	}
	if failed {
		backoff := min(p.opts.Interval, time.Second<<min(failures, 6))
		p.log.WarnContext(ctx, "poll failed", "error", status.LastError, "failures", failures, "retry_in", backoff)
		return backoff
	}
	p.log.DebugContext(ctx, "poll done", "took", elapsed.Round(time.Millisecond), "entries", counts)
	return p.opts.Interval
}

func kindNames(ks []mikrotik.Kind) []string {
	out := make([]string, len(ks))
	for i, k := range ks {
		out[i] = string(k)
	}
	return out
}
