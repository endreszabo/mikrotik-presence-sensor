// Package mqtt publishes presence states to an MQTT broker using Home
// Assistant MQTT discovery, and keeps doing so across broker outages.
package mqtt

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/endreszabo/mikrotik-presence-sensor/internal/config"
	"github.com/endreszabo/mikrotik-presence-sensor/internal/poller"
	"github.com/endreszabo/mikrotik-presence-sensor/internal/presence"
)

const (
	qos            = 1
	publishTimeout = 10 * time.Second
	// cleanupWindow is how long retained discovery configs are collected
	// before stale ones (removed people/devices) are deleted.
	cleanupWindow = 2 * time.Second
)

// Sources supplies the data to (re)publish.
type Sources struct {
	People  []presence.Person
	Devices []string
	States  func() []presence.State
	Pollers func() []poller.Status
}

// Publisher owns the broker connection.
type Publisher struct {
	cfg     config.MQTTConfig
	topics  Topics
	src     Sources
	version string
	log     *slog.Logger
	client  paho.Client

	connected   atomic.Bool
	cleanupOnce sync.Once
	// republish serializes full-state publishes after (re)connects.
	republish sync.Mutex

	// last remembers what was published for slow-changing diagnostics so
	// that every poll does not cause a retained publish (and HA recorder
	// churn). Cleared on every (re)connect.
	lastMu sync.Mutex
	last   map[string]string
}

// New prepares a publisher; Run connects.
func New(cfg config.MQTTConfig, src Sources, version string, log *slog.Logger) (*Publisher, error) {
	p := &Publisher{
		cfg:     cfg,
		topics:  Topics{Base: cfg.BaseTopic, Node: cfg.NodeID, Prefix: cfg.DiscoveryPrefix},
		src:     src,
		version: version,
		log:     log,
	}
	opts := paho.NewClientOptions().
		AddBroker(cfg.Broker).
		SetClientID(cfg.ClientID).
		SetUsername(cfg.Username).
		SetPassword(cfg.Password).
		SetCleanSession(true).
		SetOrderMatters(false).
		SetKeepAlive(cfg.Keepalive).
		SetConnectTimeout(cfg.ConnectTimeout).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(5*time.Second).
		SetMaxReconnectInterval(cfg.MaxReconnectInterval).
		SetWill(p.topics.Availability(), "offline", qos, true).
		SetOnConnectHandler(p.onConnect).
		SetConnectionLostHandler(func(_ paho.Client, err error) {
			p.connected.Store(false)
			p.log.Warn("lost connection to broker, reconnecting", "broker", cfg.Broker, "error", err)
		}).
		SetReconnectingHandler(func(_ paho.Client, _ *paho.ClientOptions) {
			p.log.Debug("reconnecting to broker", "broker", cfg.Broker)
		}).
		SetConnectionAttemptHandler(func(broker *url.URL, tc *tls.Config) *tls.Config {
			p.log.Debug("connecting to broker", "broker", broker.String())
			return tc
		})
	tc, err := tlsConfig(cfg)
	if err != nil {
		return nil, err
	}
	if tc != nil {
		opts.SetTLSConfig(tc)
	}
	p.client = paho.NewClient(opts)
	return p, nil
}

func tlsConfig(cfg config.MQTTConfig) (*tls.Config, error) {
	u, err := url.Parse(cfg.Broker)
	if err != nil {
		return nil, fmt.Errorf("mqtt broker url: %w", err)
	}
	secure := map[string]bool{"ssl": true, "tls": true, "mqtts": true, "wss": true, "tcps": true}[u.Scheme]
	t := cfg.TLS
	if !secure && t.CAFile == "" && t.CertFile == "" && !t.InsecureSkipVerify {
		return nil, nil
	}
	tc := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		ServerName:         t.ServerName,
		InsecureSkipVerify: t.InsecureSkipVerify, //nolint:gosec // explicit opt-in
	}
	if t.CAFile != "" {
		pem, err := os.ReadFile(t.CAFile)
		if err != nil {
			return nil, fmt.Errorf("mqtt ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("mqtt ca_file %s: no certificates found", t.CAFile)
		}
		tc.RootCAs = pool
	}
	if t.CertFile != "" || t.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(t.CertFile, t.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("mqtt client certificate: %w", err)
		}
		tc.Certificates = []tls.Certificate{cert}
	}
	return tc, nil
}

// Connected reports whether the broker connection is up.
func (p *Publisher) Connected() bool { return p.connected.Load() }

// Run connects (retrying in the background) and blocks until ctx is done,
// then marks the bridge offline and disconnects cleanly.
func (p *Publisher) Run(ctx context.Context) error {
	p.log.Info("connecting to broker", "broker", p.cfg.Broker, "client_id", p.cfg.ClientID)
	p.client.Connect() // with ConnectRetry the token only completes once connected
	<-ctx.Done()
	if p.client.IsConnectionOpen() {
		p.client.Publish(p.topics.Availability(), qos, true, "offline").WaitTimeout(time.Second)
	}
	p.client.Disconnect(250)
	p.connected.Store(false)
	p.log.Info("disconnected from broker")
	return nil
}

func (p *Publisher) onConnect(c paho.Client) {
	p.connected.Store(true)
	p.log.Info("connected to broker", "broker", p.cfg.Broker)
	// Subscriptions do not survive a clean session; renew them every time.
	c.Subscribe(p.topics.HAStatus(), qos, func(_ paho.Client, m paho.Message) {
		if string(m.Payload()) == "online" {
			p.log.Info("home assistant (re)started, republishing discovery and states")
			go p.publishAll()
		}
	})
	p.cleanupOnce.Do(func() { go p.cleanupStale() })
	p.publishAll()
}

// publishAll sends availability, discovery and every current state. It is
// the recovery path after outages, so states changed meanwhile are not lost.
func (p *Publisher) publishAll() {
	p.republish.Lock()
	defer p.republish.Unlock()
	p.lastMu.Lock()
	p.last = map[string]string{}
	p.lastMu.Unlock()
	p.publish(p.topics.Availability(), []byte("online"))
	for _, m := range Discovery(p.topics, p.src.People, p.src.Devices, p.version) {
		p.publish(m.Topic, m.Payload)
	}
	for _, s := range p.src.States() {
		p.PublishPerson(s)
	}
	for _, s := range p.src.Pollers() {
		p.publishDevice(s)
	}
	p.publishBridge()
}

// cleanupStale deletes retained discovery configs of this node that the
// current configuration no longer produces (removed people or devices).
func (p *Publisher) cleanupStale() {
	want := map[string]bool{}
	for _, m := range Discovery(p.topics, p.src.People, p.src.Devices, p.version) {
		want[m.Topic] = true
	}
	var mu sync.Mutex
	var stale []string
	tok := p.client.Subscribe(p.topics.DiscoveryWildcard(), qos, func(_ paho.Client, m paho.Message) {
		if m.Retained() && len(m.Payload()) > 0 && !want[m.Topic()] {
			mu.Lock()
			stale = append(stale, m.Topic())
			mu.Unlock()
		}
	})
	if !tok.WaitTimeout(publishTimeout) || tok.Error() != nil {
		p.log.Warn("could not scan for stale discovery entries", "error", tok.Error())
		return
	}
	time.Sleep(cleanupWindow)
	p.client.Unsubscribe(p.topics.DiscoveryWildcard()).WaitTimeout(publishTimeout)
	mu.Lock()
	defer mu.Unlock()
	for _, topic := range slices.Compact(slices.Sorted(slices.Values(stale))) {
		p.log.Info("removing stale discovery entry", "topic", topic)
		p.publish(topic, nil)
	}
}

func (p *Publisher) publish(topic string, payload []byte) {
	if !p.connected.Load() {
		return // everything is republished on reconnect
	}
	tok := p.client.Publish(topic, qos, true, payload)
	go func() {
		if !tok.WaitTimeout(publishTimeout) {
			p.log.Warn("publish timed out", "topic", topic)
		} else if err := tok.Error(); err != nil {
			p.log.Warn("publish failed", "topic", topic, "error", err)
		}
	}()
}

func (p *Publisher) publishJSON(topic string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		p.log.Error("encoding payload", "topic", topic, "error", err)
		return
	}
	p.publish(topic, b)
}

// personAttributes is the json_attributes payload of a person.
type personAttributes struct {
	presence.Attributes
	Since time.Time `json:"since,omitzero"`
}

// PublishPerson publishes a person's state and attributes. Unknown states
// (right after startup) are not published so retained values stay intact.
func (p *Publisher) PublishPerson(s presence.State) {
	if s.Status == presence.StatusUnknown {
		return
	}
	p.publishJSON(p.topics.PersonAttributes(s.ID), personAttributes{Attributes: s.Attributes, Since: s.Since})
	p.publish(p.topics.PersonState(s.ID), []byte(s.Status))
	p.publishBridge()
}

// PublishDevice publishes a poller status.
func (p *Publisher) PublishDevice(s poller.Status) {
	p.publishDevice(s)
	p.publishBridge()
}

func (p *Publisher) publishDevice(s poller.Status) {
	if s.LastAttempt.IsZero() {
		return
	}
	state := "OFF"
	if s.OK {
		state = "ON"
	}
	if !p.changed("device/"+s.Device, fmt.Sprint(state, s.LastError, s.Kinds)) {
		return
	}
	p.publishJSON(p.topics.DeviceAttributes(s.Device), s)
	p.publish(p.topics.DeviceState(s.Device), []byte(state))
}

// changed records value under key and reports whether it differs from the
// previously published one. Nothing is recorded while disconnected.
func (p *Publisher) changed(key, value string) bool {
	if !p.connected.Load() {
		return false
	}
	p.lastMu.Lock()
	defer p.lastMu.Unlock()
	if p.last == nil {
		p.last = map[string]string{}
	}
	if old, ok := p.last[key]; ok && old == value {
		return false
	}
	p.last[key] = value
	return true
}

type bridgeState struct {
	PeopleHome   int      `json:"people_home"`
	Home         []string `json:"home"`
	DevicesOK    int      `json:"devices_ok"`
	DevicesTotal int      `json:"devices_total"`
	Version      string   `json:"version"`
}

func (p *Publisher) publishBridge() {
	b := bridgeState{Home: []string{}, Version: p.version}
	for _, s := range p.src.States() {
		if s.Home() {
			b.PeopleHome++
			b.Home = append(b.Home, s.ID)
		}
	}
	for _, s := range p.src.Pollers() {
		b.DevicesTotal++
		if s.OK {
			b.DevicesOK++
		}
	}
	if p.changed("bridge", fmt.Sprint(b)) {
		p.publishJSON(p.topics.BridgeState(), b)
	}
}
