// Package health serves liveness, readiness, diagnostic and metrics endpoints, and
// implements the client side used by the container HEALTHCHECK.
package health

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Checks supplies the data the endpoints report.
type Checks struct {
	// RequireMQTT makes /healthz fail while MQTT is disconnected.
	RequireMQTT bool
	MQTT        func() bool
	// Ready reports whether at least one device has been polled successfully
	// (or no polling is configured).
	Ready  func() bool
	Status func() any
	// Metrics, if set, is served on /metrics.
	Metrics http.Handler
}

// Server is the HTTP health server.
type Server struct {
	listen string
	checks Checks
	log    *slog.Logger
	srv    *http.Server
}

// New creates the server.
func New(listen string, checks Checks, log *slog.Logger) *Server {
	s := &Server{listen: listen, checks: checks, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET /status", s.status)
	if checks.Metrics != nil {
		mux.Handle("GET /metrics", checks.Metrics)
	}
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	return s
}

// Handler exposes the routes (for tests).
func (s *Server) Handler() http.Handler { return s.srv.Handler }

type result struct {
	Status string `json:"status"`
	MQTT   bool   `json:"mqtt_connected"`
	Ready  bool   `json:"ready,omitempty"`
	Reason string `json:"reason,omitempty"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	r := result{Status: "ok", MQTT: s.checks.MQTT()}
	if s.checks.RequireMQTT && !r.MQTT {
		r.Status, r.Reason = "unhealthy", "mqtt disconnected"
		writeJSON(w, http.StatusServiceUnavailable, r)
		return
	}
	writeJSON(w, http.StatusOK, r)
}

func (s *Server) readyz(w http.ResponseWriter, _ *http.Request) {
	r := result{Status: "ok", MQTT: s.checks.MQTT(), Ready: s.checks.Ready()}
	switch {
	case !r.MQTT:
		r.Status, r.Reason = "not ready", "mqtt disconnected"
	case !r.Ready:
		r.Status, r.Reason = "not ready", "no successful device poll yet"
	default:
		writeJSON(w, http.StatusOK, r)
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, r)
}

func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.checks.Status())
}

// Run serves until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.listen)
	if err != nil {
		return fmt.Errorf("health listener: %w", err)
	}
	s.log.Info("health endpoint listening", "addr", ln.Addr().String())
	errc := make(chan error, 1)
	go func() { errc <- s.srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// ProbeURLs derives the URLs a local health probe should try for a listen
// address. Wildcard listeners are probed on loopback, IPv6 first, with an
// IPv4 fallback for containers that have IPv6 disabled.
func ProbeURLs(listen, path string) ([]string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return nil, err
	}
	var hosts []string
	switch host {
	case "", "::":
		hosts = []string{"::1", "127.0.0.1"}
	case "0.0.0.0":
		hosts = []string{"127.0.0.1"}
	default:
		hosts = []string{host}
	}
	urls := make([]string, len(hosts))
	for i, h := range hosts {
		urls[i] = "http://" + net.JoinHostPort(h, port) + path
	}
	return urls, nil
}

// Probe GETs each URL in turn and succeeds on the first HTTP 200.
func Probe(ctx context.Context, urls []string) error {
	client := &http.Client{Timeout: 3 * time.Second}
	var errs []error
	for _, u := range urls {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return nil
		}
		errs = append(errs, fmt.Errorf("%s: HTTP %d", u, resp.StatusCode))
	}
	return errors.Join(errs...)
}
