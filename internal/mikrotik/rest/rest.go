// Package rest polls RouterOS 7 devices through the REST API (/rest), which is
// only served over HTTPS (the www-ssl service).
package rest

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/endreszabo/mikrotik-presence-sensor/internal/macaddr"
	"github.com/endreszabo/mikrotik-presence-sensor/internal/mikrotik"
)

// Options configures a Client.
type Options struct {
	Device             string
	Host               string
	Port               int
	Username           string
	Password           string
	CAFile             string
	InsecureSkipVerify bool
	ServerName         string
	Timeout            time.Duration
}

// Client is a RouterOS REST client. It is safe for sequential use by a poller.
type Client struct {
	opts Options
	base *url.URL
	http *http.Client
	now  func() time.Time
}

type record map[string]any

func (r record) str(key string) string {
	switch v := r[key].(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

func (r record) bool(key string) bool {
	return r.str(key) == "true"
}

// signal parses "-62", "-62@6Mbps" or "-62dBm" into an integer.
func (r record) signal(key string) *int {
	s := r.str(key)
	if i := strings.IndexFunc(s, func(c rune) bool { return c != '-' && (c < '0' || c > '9') }); i >= 0 {
		s = s[:i]
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return nil
	}
	return &n
}

type endpoint struct {
	path  string
	props []string
	parse func(c *Client, r record, now time.Time) (mikrotik.Observation, bool)
}

var endpoints = map[mikrotik.Kind]endpoint{
	mikrotik.KindWifi: {
		path:  "interface/wifi/registration-table",
		props: []string{"mac-address", "interface", "ssid", "signal", "band"},
		parse: func(c *Client, r record, now time.Time) (mikrotik.Observation, bool) {
			return c.obs(mikrotik.KindWifi, r.str("mac-address"), now, func(o *mikrotik.Observation) {
				o.Interface, o.SSID, o.Band, o.Signal = r.str("interface"), r.str("ssid"), r.str("band"), r.signal("signal")
			})
		},
	},
	mikrotik.KindWireless: {
		path:  "interface/wireless/registration-table",
		props: []string{"mac-address", "interface", "signal-strength", "ap"},
		parse: func(c *Client, r record, now time.Time) (mikrotik.Observation, bool) {
			if r.bool("ap") { // a peer AP in WDS/station-bridge, not a client
				return mikrotik.Observation{}, false
			}
			return c.obs(mikrotik.KindWireless, r.str("mac-address"), now, func(o *mikrotik.Observation) {
				o.Interface, o.Signal = r.str("interface"), r.signal("signal-strength")
			})
		},
	},
	mikrotik.KindCapsman: {
		path:  "caps-man/registration-table",
		props: []string{"mac-address", "interface", "ssid", "rx-signal"},
		parse: func(c *Client, r record, now time.Time) (mikrotik.Observation, bool) {
			return c.obs(mikrotik.KindCapsman, r.str("mac-address"), now, func(o *mikrotik.Observation) {
				o.Interface, o.SSID, o.Signal = r.str("interface"), r.str("ssid"), r.signal("rx-signal")
			})
		},
	},
	mikrotik.KindARP: {
		path:  "ip/arp",
		props: []string{"address", "mac-address", "interface", "status", "complete", "invalid", "disabled"},
		parse: func(c *Client, r record, now time.Time) (mikrotik.Observation, bool) {
			if r.bool("invalid") || r.bool("disabled") || r.str("complete") == "false" {
				return mikrotik.Observation{}, false
			}
			return c.obs(mikrotik.KindARP, r.str("mac-address"), now, func(o *mikrotik.Observation) {
				o.Interface, o.IP, o.State = r.str("interface"), r.str("address"), r.str("status")
			})
		},
	},
	mikrotik.KindNeighbor: {
		path:  "ipv6/neighbor",
		props: []string{"address", "mac-address", "interface", "status"},
		parse: func(c *Client, r record, now time.Time) (mikrotik.Observation, bool) {
			return c.obs(mikrotik.KindNeighbor, r.str("mac-address"), now, func(o *mikrotik.Observation) {
				o.Interface, o.IP, o.State = r.str("interface"), r.str("address"), r.str("status")
			})
		},
	},
	mikrotik.KindBridgeHost: {
		path:  "interface/bridge/host",
		props: []string{"mac-address", "on-interface", "bridge", "local", "invalid"},
		parse: func(c *Client, r record, now time.Time) (mikrotik.Observation, bool) {
			if r.bool("local") || r.bool("invalid") {
				return mikrotik.Observation{}, false
			}
			return c.obs(mikrotik.KindBridgeHost, r.str("mac-address"), now, func(o *mikrotik.Observation) {
				o.Interface = r.str("on-interface")
			})
		},
	},
	mikrotik.KindDHCP: {
		path:  "ip/dhcp-server/lease",
		props: []string{"active-mac-address", "mac-address", "active-address", "address", "server", "status", "last-seen"},
		parse: func(c *Client, r record, now time.Time) (mikrotik.Observation, bool) {
			if r.str("status") != "bound" {
				return mikrotik.Observation{}, false
			}
			mac, ip := r.str("active-mac-address"), r.str("active-address")
			if mac == "" {
				mac = r.str("mac-address")
			}
			if ip == "" {
				ip = r.str("address")
			}
			// The lease table holds bound leases for their whole lifetime;
			// last-seen tells when the client actually talked to the server.
			seen := now
			if d, err := mikrotik.ParseDuration(r.str("last-seen")); err == nil {
				seen = now.Add(-d)
			}
			return c.obs(mikrotik.KindDHCP, mac, seen, func(o *mikrotik.Observation) {
				o.Interface, o.IP, o.State = r.str("server"), ip, "bound"
			})
		},
	},
}

// New returns a client for one device.
func New(opts Options) (*Client, error) {
	tlsCfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		ServerName:         opts.ServerName,
		InsecureSkipVerify: opts.InsecureSkipVerify, //nolint:gosec // explicit opt-in for self-signed RouterOS certificates
	}
	if opts.CAFile != "" {
		pem, err := os.ReadFile(opts.CAFile)
		if err != nil {
			return nil, fmt.Errorf("reading CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificates found in %s", opts.CAFile)
		}
		tlsCfg.RootCAs = pool
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Second
	}
	transport := &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: opts.Timeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:     tlsCfg,
		TLSHandshakeTimeout: opts.Timeout,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     90 * time.Second,
	}
	return &Client{
		opts: opts,
		base: &url.URL{Scheme: "https", Host: net.JoinHostPort(opts.Host, strconv.Itoa(opts.Port)), Path: "/rest/"},
		http: &http.Client{Transport: transport, Timeout: opts.Timeout},
		now:  time.Now,
	}, nil
}

// BaseURL returns the REST root, e.g. https://[2001:db8::1]:443/rest/.
func (c *Client) BaseURL() string { return c.base.String() }

// Kinds implements mikrotik.Client.
func (c *Client) Kinds() []mikrotik.Kind { return mikrotik.AllKinds() }

// Close implements mikrotik.Client.
func (c *Client) Close() error {
	c.http.CloseIdleConnections()
	return nil
}

// Poll implements mikrotik.Client.
func (c *Client) Poll(ctx context.Context, kinds []mikrotik.Kind) mikrotik.Result {
	res := mikrotik.NewResult()
	for _, k := range kinds {
		ep, ok := endpoints[k]
		if !ok {
			res.Errors[k] = fmt.Errorf("%s: %w by the REST transport", k, mikrotik.ErrUnsupported)
			continue
		}
		start := time.Now()
		recs, err := c.get(ctx, ep.path, ep.props)
		res.Durations[k] = time.Since(start)
		if err != nil {
			res.Errors[k] = err
			if errors.Is(err, errAuth) {
				// Every further request would fail the same way.
				for _, rest := range kinds {
					if _, done := res.Errors[rest]; !done && res.Snapshots[rest] == nil {
						res.Errors[rest] = err
					}
				}
				return res
			}
			continue
		}
		now := c.now()
		obs := make([]mikrotik.Observation, 0, len(recs))
		for _, r := range recs {
			if o, ok := ep.parse(c, r, now); ok {
				obs = append(obs, o)
			}
		}
		res.Snapshots[k] = obs
	}
	return res
}

func (c *Client) obs(kind mikrotik.Kind, rawMAC string, seen time.Time, fill func(*mikrotik.Observation)) (mikrotik.Observation, bool) {
	mac, err := macaddr.Normalize(rawMAC)
	if err != nil {
		return mikrotik.Observation{}, false
	}
	o := mikrotik.Observation{MAC: mac, Kind: kind, Device: c.opts.Device, SeenAt: seen}
	fill(&o)
	return o, true
}

var errAuth = errors.New("authentication failed")

type apiError struct {
	Error   int    `json:"error"`
	Message string `json:"message"`
	Detail  string `json:"detail"`
}

func (c *Client) get(ctx context.Context, path string, props []string) ([]record, error) {
	u := c.base.JoinPath(path)
	u.RawQuery = url.Values{".proplist": {strings.Join(props, ",")}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.opts.Username, c.opts.Password)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("%s: reading response: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		var ae apiError
		_ = json.Unmarshal(body, &ae)
		detail := strings.TrimSpace(ae.Detail + " " + ae.Message)
		switch {
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			return nil, fmt.Errorf("%s: %w (HTTP %d %s)", path, errAuth, resp.StatusCode, detail)
		case resp.StatusCode == http.StatusNotFound ||
			resp.StatusCode == http.StatusBadRequest && strings.Contains(ae.Detail, "no such command"):
			return nil, fmt.Errorf("%s: %w on this device (HTTP %d %s)", path, mikrotik.ErrUnsupported, resp.StatusCode, detail)
		default:
			return nil, fmt.Errorf("%s: HTTP %d %s", path, resp.StatusCode, detail)
		}
	}
	var recs []record
	if err := json.Unmarshal(body, &recs); err != nil {
		return nil, fmt.Errorf("%s: decoding response: %w", path, err)
	}
	return recs, nil
}
