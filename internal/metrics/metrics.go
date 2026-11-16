// Package metrics exposes Prometheus metrics about monitored devices, polling,
// syslog ingestion and presence.
package metrics

import (
	"net/http"
	"runtime"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/endreszabo/mikrotik-presence-sensor/internal/poller"
	"github.com/endreszabo/mikrotik-presence-sensor/internal/presence"
	"github.com/endreszabo/mikrotik-presence-sensor/internal/syslog"
)

const namespace = "mikrotik_presence"

// Metrics holds all collectors in a private registry.
type Metrics struct {
	reg *prometheus.Registry

	deviceUp          *prometheus.GaugeVec
	deviceLastSuccess *prometheus.GaugeVec
	deviceFailures    *prometheus.GaugeVec
	polls             *prometheus.CounterVec
	pollDuration      *prometheus.HistogramVec
	kindDuration      *prometheus.HistogramVec
	kindErrors        *prometheus.CounterVec
	discovered        *prometheus.GaugeVec

	syslogMessages *prometheus.CounterVec
	syslogEvents   *prometheus.CounterVec
	syslogDropped  prometheus.Counter
	syslogLast     *prometheus.GaugeVec

	personHome *prometheus.GaugeVec
}

// pollBuckets span fast REST calls on a LAN to slow SNMP walks of big tables.
var pollBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30}

// New creates the metrics. mqttConnected and peopleHome are sampled on scrape.
func New(version string, mqttConnected func() bool, peopleHome func() int) *Metrics {
	m := &Metrics{reg: prometheus.NewRegistry()}
	f := factory{m.reg}

	f.gaugeFunc("build_info", "Build information; always 1.", prometheus.Labels{"version": version, "goversion": runtime.Version()},
		func() float64 { return 1 })
	f.gaugeFunc("mqtt_connected", "1 if the MQTT broker connection is up.", nil, func() float64 { return b2f(mqttConnected()) })
	f.gaugeFunc("people_home", "Number of people currently home.", nil, func() float64 { return float64(peopleHome()) })

	m.deviceUp = f.gaugeVec("device_up", "1 if the last poll of the device succeeded for every table, 0 otherwise.", "device", "transport")
	m.deviceLastSuccess = f.gaugeVec("device_last_success_timestamp_seconds", "Unix time of the last fully successful poll.", "device")
	m.deviceFailures = f.gaugeVec("device_consecutive_failures", "Number of consecutive failed polls.", "device")
	m.polls = f.counterVec("polls_total", "Poll cycles by result (success or error).", "device", "result")
	m.pollDuration = f.histogramVec("poll_duration_seconds", "Duration of a whole poll cycle of a device.", "device")
	m.kindDuration = f.histogramVec("poll_kind_duration_seconds", "Time spent fetching one table (kind) of a device.", "device", "kind")
	m.kindErrors = f.counterVec("poll_kind_errors_total", "Failed fetches of one table (kind) of a device.", "device", "kind")
	m.discovered = f.gaugeVec("discovered_items", "Entries in a device table (kind) at the last successful fetch, tracked or not.", "device", "kind")

	m.syslogMessages = f.counterVec("syslog_messages_total", "Syslog messages from allowed senders by result (matched a rule or ignored).", "device", "result")
	m.syslogEvents = f.counterVec("syslog_events_total", "Presence events parsed from syslog.", "device", "kind", "event")
	m.syslogDropped = f.counter("syslog_dropped_total", "Syslog messages dropped because the sender is not allowed.")
	m.syslogLast = f.gaugeVec("syslog_last_message_timestamp_seconds", "Unix time of the last syslog message from the device.", "device")

	m.personHome = f.gaugeVec("person_home", "1 if the person is home, 0 if not (absent while unknown after startup).", "person")

	m.reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

// Handler serves the registry in the Prometheus exposition format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{Registry: m.reg})
}

// ObservePoll records a finished poll cycle (poller.OnReport).
func (m *Metrics) ObservePoll(r poller.Report) {
	s := r.Status
	m.deviceUp.WithLabelValues(s.Device, s.Transport).Set(b2f(s.OK))
	m.deviceFailures.WithLabelValues(s.Device).Set(float64(s.ConsecutiveFailures))
	if !s.LastSuccess.IsZero() {
		m.deviceLastSuccess.WithLabelValues(s.Device).Set(float64(s.LastSuccess.UnixMilli()) / 1e3)
	}
	result := "success"
	if !s.OK {
		result = "error"
	}
	m.polls.WithLabelValues(s.Device, result).Inc()
	m.pollDuration.WithLabelValues(s.Device).Observe(r.Elapsed.Seconds())
	for _, k := range r.Kinds {
		m.kindDuration.WithLabelValues(s.Device, string(k.Kind)).Observe(k.Duration.Seconds())
		if k.Err != nil {
			m.kindErrors.WithLabelValues(s.Device, string(k.Kind)).Inc()
			continue
		}
		m.discovered.WithLabelValues(s.Device, string(k.Kind)).Set(float64(k.Items))
	}
}

// Message implements syslog.Observer.
func (m *Metrics) Message(device string, match *syslog.Match) {
	m.syslogLast.WithLabelValues(device).SetToCurrentTime()
	if match == nil {
		m.syslogMessages.WithLabelValues(device, "ignored").Inc()
		return
	}
	m.syslogMessages.WithLabelValues(device, "matched").Inc()
	kind := string(match.Kind)
	if kind == "" {
		kind = "log"
	}
	m.syslogEvents.WithLabelValues(device, kind, string(match.Event)).Inc()
}

// Dropped implements syslog.Observer.
func (m *Metrics) Dropped() { m.syslogDropped.Inc() }

// PersonChanged records a person's state (presence.Engine.OnChange).
func (m *Metrics) PersonChanged(s presence.State) {
	switch s.Status {
	case presence.StatusHome:
		m.personHome.WithLabelValues(s.ID).Set(1)
	case presence.StatusNotHome:
		m.personHome.WithLabelValues(s.ID).Set(0)
	}
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// factory registers collectors with the namespace applied.
type factory struct{ reg *prometheus.Registry }

func (p factory) gaugeVec(name, help string, labels ...string) *prometheus.GaugeVec {
	v := prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: name, Help: help}, labels)
	p.reg.MustRegister(v)
	return v
}

func (p factory) counterVec(name, help string, labels ...string) *prometheus.CounterVec {
	v := prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: namespace, Name: name, Help: help}, labels)
	p.reg.MustRegister(v)
	return v
}

func (p factory) counter(name, help string) prometheus.Counter {
	c := prometheus.NewCounter(prometheus.CounterOpts{Namespace: namespace, Name: name, Help: help})
	p.reg.MustRegister(c)
	return c
}

func (p factory) histogramVec(name, help string, labels ...string) *prometheus.HistogramVec {
	v := prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: namespace, Name: name, Help: help, Buckets: pollBuckets}, labels)
	p.reg.MustRegister(v)
	return v
}

func (p factory) gaugeFunc(name, help string, constLabels prometheus.Labels, f func() float64) {
	p.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Namespace: namespace, Name: name, Help: help, ConstLabels: constLabels}, f))
}
