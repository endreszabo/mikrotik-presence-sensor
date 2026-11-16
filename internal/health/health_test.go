package health

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestEndpoints(t *testing.T) {
	mqttUp, ready := false, false
	s := New("[::1]:0", Checks{
		RequireMQTT: true,
		MQTT:        func() bool { return mqttUp },
		Ready:       func() bool { return ready },
		Status:      func() any { return map[string]string{"hello": "world"} },
		Metrics: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("up 1\n"))
		}),
	}, slog.New(slog.DiscardHandler))
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	code := func(path string) int {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := code("/healthz"); c != 503 {
		t.Errorf("healthz with mqtt down = %d", c)
	}
	mqttUp = true
	if c := code("/healthz"); c != 200 {
		t.Errorf("healthz = %d", c)
	}
	if c := code("/readyz"); c != 503 {
		t.Errorf("readyz before first poll = %d", c)
	}
	ready = true
	if c := code("/readyz"); c != 200 {
		t.Errorf("readyz = %d", c)
	}
	if c := code("/status"); c != 200 {
		t.Errorf("status = %d", c)
	}
	if c := code("/metrics"); c != 200 {
		t.Errorf("metrics = %d", c)
	}
	if err := Probe(t.Context(), []string{"http://[::1]:1/healthz", srv.URL + "/healthz"}); err != nil {
		t.Errorf("Probe with fallback: %v", err)
	}
	mqttUp = false
	if err := Probe(t.Context(), []string{srv.URL + "/healthz"}); err == nil {
		t.Error("Probe succeeded on 503")
	}
}

func TestProbeURLs(t *testing.T) {
	for listen, want := range map[string][]string{
		"[::]:8080":        {"http://[::1]:8080/healthz", "http://127.0.0.1:8080/healthz"},
		":8080":            {"http://[::1]:8080/healthz", "http://127.0.0.1:8080/healthz"},
		"0.0.0.0:9":        {"http://127.0.0.1:9/healthz"},
		"[2001:db8::1]:80": {"http://[2001:db8::1]:80/healthz"},
	} {
		got, err := ProbeURLs(listen, "/healthz")
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("ProbeURLs(%q) = %v, %v", listen, got, err)
		}
	}
}
