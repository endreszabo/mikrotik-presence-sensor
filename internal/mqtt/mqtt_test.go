package mqtt

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/endreszabo/mikrotik-presence-sensor/internal/config"
	"github.com/endreszabo/mikrotik-presence-sensor/internal/poller"
	"github.com/endreszabo/mikrotik-presence-sensor/internal/presence"
)

var testTopics = Topics{Base: "mikrotik-presence", Node: "mp", Prefix: "homeassistant"}

func TestDiscovery(t *testing.T) {
	msgs := Discovery(testTopics,
		[]presence.Person{{ID: "alice", Name: "Alice", MACs: []string{"aa:bb:cc:dd:ee:01"}}},
		[]string{"AP Living Room"}, "1.2.3")
	byTopic := map[string]map[string]any{}
	for _, m := range msgs {
		var v map[string]any
		if err := json.Unmarshal(m.Payload, &v); err != nil {
			t.Fatalf("%s: %v", m.Topic, err)
		}
		byTopic[m.Topic] = v
	}
	if len(byTopic) != len(msgs) {
		t.Fatal("duplicate discovery topics")
	}

	tr := byTopic["homeassistant/device_tracker/mp/alice/config"]
	if tr == nil {
		t.Fatalf("no tracker config; topics: %v", keys(byTopic))
	}
	if v, ok := tr["name"]; !ok || v != nil {
		t.Errorf("tracker name = %v, want explicit null", v)
	}
	for k, want := range map[string]any{
		"unique_id":             "mp_alice_tracker",
		"state_topic":           "mikrotik-presence/mp/person/alice/state",
		"json_attributes_topic": "mikrotik-presence/mp/person/alice/attributes",
		"payload_home":          "home",
		"payload_not_home":      "not_home",
		"source_type":           "router",
		"availability_topic":    "mikrotik-presence/mp/availability",
	} {
		if tr[k] != want {
			t.Errorf("tracker %s = %v, want %v", k, tr[k], want)
		}
	}
	dev := tr["device"].(map[string]any)
	if dev["via_device"] != "mp_bridge" || dev["name"] != "Alice presence" {
		t.Errorf("tracker device = %v", dev)
	}

	bs := byTopic["homeassistant/binary_sensor/mp/alice_presence/config"]
	if bs["device_class"] != "presence" || bs["payload_on"] != "home" || bs["payload_off"] != "not_home" {
		t.Errorf("binary sensor = %v", bs)
	}
	poll := byTopic["homeassistant/binary_sensor/mp/device_ap_living_room_poll/config"]
	if poll == nil || poll["state_topic"] != "mikrotik-presence/mp/device/ap_living_room/state" || poll["entity_category"] != "diagnostic" {
		t.Errorf("device poll sensor = %v (topics %v)", poll, keys(byTopic))
	}
	for _, topic := range []string{
		"homeassistant/binary_sensor/mp/bridge_connectivity/config",
		"homeassistant/sensor/mp/bridge_people_home/config",
		"homeassistant/sensor/mp/bridge_devices_ok/config",
	} {
		if byTopic[topic] == nil {
			t.Errorf("missing %s", topic)
		}
	}
}

func keys(m map[string]map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{"AP Living Room": "ap_living_room", "core-rtr.lan": "core-rtr_lan", "--x--": "x"} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTLSConfig(t *testing.T) {
	if tc, err := tlsConfig(config.MQTTConfig{Broker: "tcp://[::1]:1883"}); err != nil || tc != nil {
		t.Errorf("plain broker: %v %v", tc, err)
	}
	if tc, err := tlsConfig(config.MQTTConfig{Broker: "ssl://[2001:db8::1]:8883", TLS: config.TLSConfig{ServerName: "mqtt.lan"}}); err != nil || tc == nil || tc.ServerName != "mqtt.lan" {
		t.Errorf("ssl broker: %v %v", tc, err)
	}
	if _, err := tlsConfig(config.MQTTConfig{Broker: "ssl://x:8883", TLS: config.TLSConfig{CAFile: "/nonexistent"}}); err == nil {
		t.Error("missing CA file accepted")
	}
}

// TestBroker runs against a real broker when MPS_TEST_BROKER is set, e.g.
// MPS_TEST_BROKER=tcp://[::1]:1883 go test ./internal/mqtt -run Broker
func TestBroker(t *testing.T) {
	broker := os.Getenv("MPS_TEST_BROKER")
	if broker == "" {
		t.Skip("MPS_TEST_BROKER not set")
	}
	cfg := config.Default().MQTT
	cfg.Broker, cfg.ClientID, cfg.NodeID, cfg.BaseTopic = broker, "mps-test", "mps_test", "mps-test"
	states := []presence.State{{ID: "alice", Name: "Alice", Status: presence.StatusHome}}
	p, err := New(cfg, Sources{
		People:  []presence.Person{{ID: "alice", Name: "Alice"}},
		Devices: []string{"ap1"},
		States:  func() []presence.State { return states },
		Pollers: func() []poller.Status {
			return []poller.Status{{Device: "ap1", OK: true, LastAttempt: time.Now()}}
		},
	}, "test", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	got := map[string]string{}
	sub := paho.NewClient(paho.NewClientOptions().AddBroker(broker).SetClientID("mps-test-sub"))
	if tok := sub.Connect(); !tok.WaitTimeout(5*time.Second) || tok.Error() != nil {
		t.Fatalf("subscriber connect: %v", tok.Error())
	}
	defer sub.Disconnect(100)
	sub.Subscribe("#", 1, func(_ paho.Client, m paho.Message) {
		if strings.HasPrefix(m.Topic(), "mps-test/") || strings.Contains(m.Topic(), "/mps_test/") {
			mu.Lock()
			got[m.Topic()] = string(m.Payload())
			mu.Unlock()
		}
	}).Wait()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = p.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		ok := got["mps-test/mps_test/person/alice/state"] == "home" &&
			got["mps-test/mps_test/availability"] == "online" &&
			got["homeassistant/device_tracker/mps_test/alice/config"] != "" &&
			strings.Contains(got["mps-test/mps_test/bridge/state"], `"people_home":1`)
		mu.Unlock()
		if ok {
			break
		}
		if time.Now().After(deadline) {
			mu.Lock()
			t.Fatalf("missing messages, got %v", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !p.Connected() {
		t.Error("Connected() = false")
	}
	// Clean up retained test messages.
	sub.Unsubscribe("#").Wait()
	mu.Lock()
	defer mu.Unlock()
	for topic := range got {
		sub.Publish(topic, 1, true, []byte{}).Wait()
	}
}
