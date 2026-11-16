package metrics

import (
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/endreszabo/mikrotik-presence-sensor/internal/mikrotik"
	"github.com/endreszabo/mikrotik-presence-sensor/internal/poller"
	"github.com/endreszabo/mikrotik-presence-sensor/internal/presence"
	"github.com/endreszabo/mikrotik-presence-sensor/internal/syslog"
)

func TestMetrics(t *testing.T) {
	m := New("1.0", func() bool { return true }, func() int { return 2 })

	ok := time.Unix(1700000000, 0)
	m.ObservePoll(poller.Report{
		Status:  poller.Status{Device: "ap1", Transport: "rest", OK: true, LastSuccess: ok, LastAttempt: ok},
		Elapsed: 40 * time.Millisecond,
		Kinds: []poller.KindReport{
			{Kind: mikrotik.KindWifi, Duration: 20 * time.Millisecond, Items: 7},
			{Kind: mikrotik.KindARP, Duration: 15 * time.Millisecond, Items: 31},
		},
	})
	m.ObservePoll(poller.Report{
		Status: poller.Status{Device: "sw1", Transport: "snmp", ConsecutiveFailures: 2, LastAttempt: ok},
		Kinds:  []poller.KindReport{{Kind: mikrotik.KindBridgeHost, Duration: 5 * time.Second, Err: errors.New("timeout")}},
	})
	m.Message("ap1", &syslog.Match{Event: presence.EventJoin, Kind: mikrotik.KindWifi})
	m.Message("ap1", &syslog.Match{Event: presence.EventSeen})
	m.Message("ap1", nil)
	m.Dropped()
	m.PersonChanged(presence.State{ID: "alice", Status: presence.StatusHome})
	m.PersonChanged(presence.State{ID: "bob", Status: presence.StatusUnknown})

	checks := []struct {
		name      string
		got, want float64
	}{
		{`m.deviceUp.WithLabelValues("ap1", "rest")`, testutil.ToFloat64(m.deviceUp.WithLabelValues("ap1", "rest")), 1},
		{`m.deviceUp.WithLabelValues("sw1", "snmp")`, testutil.ToFloat64(m.deviceUp.WithLabelValues("sw1", "snmp")), 0},
		{`m.deviceLastSuccess.WithLabelValues("ap1")`, testutil.ToFloat64(m.deviceLastSuccess.WithLabelValues("ap1")), 1700000000},
		{`m.deviceFailures.WithLabelValues("sw1")`, testutil.ToFloat64(m.deviceFailures.WithLabelValues("sw1")), 2},
		{`m.discovered.WithLabelValues("ap1", "arp")`, testutil.ToFloat64(m.discovered.WithLabelValues("ap1", "arp")), 31},
		{`m.kindErrors.WithLabelValues("sw1", "bridge_host")`, testutil.ToFloat64(m.kindErrors.WithLabelValues("sw1", "bridge_host")), 1},
		{`m.polls.WithLabelValues("sw1", "error")`, testutil.ToFloat64(m.polls.WithLabelValues("sw1", "error")), 1},
		{`m.syslogMessages.WithLabelValues("ap1", "matched")`, testutil.ToFloat64(m.syslogMessages.WithLabelValues("ap1", "matched")), 2},
		{`m.syslogMessages.WithLabelValues("ap1", "ignored")`, testutil.ToFloat64(m.syslogMessages.WithLabelValues("ap1", "ignored")), 1},
		{`m.syslogEvents.WithLabelValues("ap1", "log", "seen")`, testutil.ToFloat64(m.syslogEvents.WithLabelValues("ap1", "log", "seen")), 1},
		{`m.syslogDropped`, testutil.ToFloat64(m.syslogDropped), 1},
		{`m.personHome.WithLabelValues("alice")`, testutil.ToFloat64(m.personHome.WithLabelValues("alice")), 1},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if n := testutil.CollectAndCount(m.personHome); n != 1 {
		t.Errorf("person_home series = %d, want 1 (unknown must not be exported)", n)
	}
	if n := testutil.CollectAndCount(m.discovered); n != 2 {
		t.Errorf("discovered_items series = %d, want 2 (failed kinds keep no value)", n)
	}

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body, _ := io.ReadAll(rec.Body)
	for _, want := range []string{
		`mikrotik_presence_build_info{goversion="`,
		`mikrotik_presence_mqtt_connected 1`,
		`mikrotik_presence_people_home 2`,
		`mikrotik_presence_poll_kind_duration_seconds_bucket{device="ap1",kind="wifi",le="0.025"} 1`,
		`mikrotik_presence_poll_duration_seconds_count{device="ap1"} 1`,
		`mikrotik_presence_syslog_events_total{device="ap1",event="join",kind="wifi"} 1`,
		`go_goroutines`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("exposition lacks %q", want)
		}
	}
}
