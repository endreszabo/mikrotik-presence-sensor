package rest

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/endreszabo/mikrotik-presence-sensor/internal/mikrotik"
)

var fixtures = map[string]string{
	"/rest/interface/wifi/registration-table": `[
		{".id":"*1","mac-address":"AA:BB:CC:DD:EE:01","interface":"wifi2","ssid":"home","signal":"-58","band":"5ghz-ax"}]`,
	"/rest/caps-man/registration-table": `[
		{".id":"*2","mac-address":"AA:BB:CC:DD:EE:03","interface":"cap-wlan1","ssid":"home","rx-signal":"-70"}]`,
	"/rest/ip/arp": `[
		{"address":"192.0.2.10","mac-address":"AA:BB:CC:DD:EE:01","interface":"bridge","status":"reachable","complete":"true"},
		{"address":"192.0.2.11","interface":"bridge","status":"failed","complete":"false"}]`,
	"/rest/ipv6/neighbor": `[
		{"address":"2001:db8::1:10","mac-address":"AA:BB:CC:DD:EE:01","interface":"bridge","status":"stale"},
		{"address":"fe80::1","mac-address":"AA:BB:CC:DD:EE:01","interface":"bridge","status":"reachable"}]`,
	"/rest/interface/bridge/host": `[
		{"mac-address":"AA:BB:CC:DD:EE:02","on-interface":"ether3","bridge":"bridge","local":"false"},
		{"mac-address":"48:8F:5A:00:00:01","on-interface":"bridge","bridge":"bridge","local":"true"}]`,
	"/rest/ip/dhcp-server/lease": `[
		{"active-mac-address":"AA:BB:CC:DD:EE:04","active-address":"192.0.2.44","server":"dhcp1","status":"bound","last-seen":"1m30s"},
		{"mac-address":"AA:BB:CC:DD:EE:05","address":"192.0.2.45","server":"dhcp1","status":"waiting","last-seen":"never"}]`,
}

func newServer(t *testing.T) (*httptest.Server, *Client) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "presence" || p != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":401,"message":"Unauthorized"}`))
			return
		}
		if r.URL.Query().Get(".proplist") == "" {
			t.Errorf("request without .proplist: %s", r.URL)
		}
		body, ok := fixtures[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":400,"message":"Bad Request","detail":"no such command or directory (wireless)"}`))
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	host, port, _ := net.SplitHostPort(u.Host)
	p, _ := strconv.Atoi(port)
	c, err := New(Options{Device: "ap1", Host: host, Port: p, Username: "presence", Password: "secret", InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	return srv, c
}

func TestPoll(t *testing.T) {
	_, c := newServer(t)
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	c.now = func() time.Time { return now }
	res := c.Poll(t.Context(), mikrotik.AllKinds())

	if err := res.Errors[mikrotik.KindWireless]; !errors.Is(err, mikrotik.ErrUnsupported) {
		t.Errorf("wireless error = %v, want ErrUnsupported", err)
	}
	wifi := res.Snapshots[mikrotik.KindWifi]
	if len(wifi) != 1 || wifi[0].MAC != "aa:bb:cc:dd:ee:01" || wifi[0].SSID != "home" ||
		wifi[0].Interface != "wifi2" || wifi[0].Signal == nil || *wifi[0].Signal != -58 || wifi[0].Device != "ap1" {
		t.Errorf("wifi = %+v", wifi)
	}
	if cm := res.Snapshots[mikrotik.KindCapsman]; len(cm) != 1 || *cm[0].Signal != -70 {
		t.Errorf("capsman = %+v", cm)
	}
	if arp := res.Snapshots[mikrotik.KindARP]; len(arp) != 1 || arp[0].IP != "192.0.2.10" || arp[0].State != "reachable" {
		t.Errorf("arp = %+v", arp)
	}
	if nb := res.Snapshots[mikrotik.KindNeighbor]; len(nb) != 2 || nb[0].State != "stale" {
		t.Errorf("neighbor = %+v", nb)
	}
	if bh := res.Snapshots[mikrotik.KindBridgeHost]; len(bh) != 1 || bh[0].Interface != "ether3" {
		t.Errorf("bridge hosts = %+v", bh)
	}
	dhcp := res.Snapshots[mikrotik.KindDHCP]
	if len(dhcp) != 1 || dhcp[0].MAC != "aa:bb:cc:dd:ee:04" || !dhcp[0].SeenAt.Equal(now.Add(-90*time.Second)) {
		t.Errorf("dhcp = %+v", dhcp)
	}
}

func TestAuthFailureStopsPolling(t *testing.T) {
	srv, _ := newServer(t)
	u, _ := url.Parse(srv.URL)
	host, port, _ := net.SplitHostPort(u.Host)
	p, _ := strconv.Atoi(port)
	c, _ := New(Options{Device: "ap1", Host: host, Port: p, Username: "presence", Password: "wrong", InsecureSkipVerify: true})
	res := c.Poll(t.Context(), []mikrotik.Kind{mikrotik.KindWifi, mikrotik.KindARP})
	for _, k := range []mikrotik.Kind{mikrotik.KindWifi, mikrotik.KindARP} {
		if !errors.Is(res.Errors[k], errAuth) {
			t.Errorf("%s error = %v, want auth error", k, res.Errors[k])
		}
	}
}

func TestTLSVerification(t *testing.T) {
	srv, _ := newServer(t)
	u, _ := url.Parse(srv.URL)
	host, port, _ := net.SplitHostPort(u.Host)
	p, _ := strconv.Atoi(port)
	c, _ := New(Options{Device: "ap1", Host: host, Port: p, Username: "presence", Password: "secret"})
	res := c.Poll(t.Context(), []mikrotik.Kind{mikrotik.KindWifi})
	if res.Errors[mikrotik.KindWifi] == nil {
		t.Fatal("expected certificate verification failure against self-signed test server")
	}
}

func TestIPv6BaseURL(t *testing.T) {
	c, err := New(Options{Host: "2001:db8::1", Port: 443})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.BaseURL(); got != "https://[2001:db8::1]:443/rest/" {
		t.Errorf("BaseURL = %q", got)
	}
}
