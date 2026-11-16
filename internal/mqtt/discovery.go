package mqtt

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/endreszabo/mikrotik-presence-sensor/internal/presence"
)

// Message is a retained MQTT message.
type Message struct {
	Topic   string
	Payload []byte
}

// Topics computes all topic names from the configuration.
type Topics struct {
	Base, Node, Prefix string
}

func (t Topics) root() string         { return t.Base + "/" + t.Node }
func (t Topics) Availability() string { return t.root() + "/availability" }
func (t Topics) BridgeState() string  { return t.root() + "/bridge/state" }
func (t Topics) PersonState(id string) string {
	return t.root() + "/person/" + id + "/state"
}
func (t Topics) PersonAttributes(id string) string {
	return t.root() + "/person/" + id + "/attributes"
}
func (t Topics) DeviceState(dev string) string {
	return t.root() + "/device/" + Slug(dev) + "/state"
}
func (t Topics) DeviceAttributes(dev string) string {
	return t.root() + "/device/" + Slug(dev) + "/attributes"
}
func (t Topics) config(component, objectID string) string {
	return t.Prefix + "/" + component + "/" + t.Node + "/" + objectID + "/config"
}

// DiscoveryWildcard matches every discovery topic owned by this node.
func (t Topics) DiscoveryWildcard() string { return t.Prefix + "/+/" + t.Node + "/+/config" }

// HAStatus is where Home Assistant announces restarts (birth message).
func (t Topics) HAStatus() string { return t.Prefix + "/status" }

var slugRe = regexp.MustCompile(`[^a-z0-9_-]+`)

// Slug turns a device name into a topic and object ID safe token.
func Slug(s string) string {
	return strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "_"), "_-")
}

type haDevice struct {
	Identifiers  []string `json:"identifiers"`
	Name         string   `json:"name"`
	Manufacturer string   `json:"manufacturer,omitempty"`
	Model        string   `json:"model,omitempty"`
	SWVersion    string   `json:"sw_version,omitempty"`
	ViaDevice    string   `json:"via_device,omitempty"`
}

type haOrigin struct {
	Name       string `json:"name"`
	SWVersion  string `json:"sw_version,omitempty"`
	SupportURL string `json:"support_url,omitempty"`
}

// entity is the union of the discovery fields used here.
type entity struct {
	// Name is nil (JSON null) for an entity named after its device.
	Name                any       `json:"name"`
	UniqueID            string    `json:"unique_id"`
	DeviceClass         string    `json:"device_class,omitempty"`
	EntityCategory      string    `json:"entity_category,omitempty"`
	Icon                string    `json:"icon,omitempty"`
	StateTopic          string    `json:"state_topic"`
	ValueTemplate       string    `json:"value_template,omitempty"`
	JSONAttributesTopic string    `json:"json_attributes_topic,omitempty"`
	PayloadHome         string    `json:"payload_home,omitempty"`
	PayloadNotHome      string    `json:"payload_not_home,omitempty"`
	PayloadOn           string    `json:"payload_on,omitempty"`
	PayloadOff          string    `json:"payload_off,omitempty"`
	SourceType          string    `json:"source_type,omitempty"`
	StateClass          string    `json:"state_class,omitempty"`
	AvailabilityTopic   string    `json:"availability_topic,omitempty"`
	Device              *haDevice `json:"device"`
	Origin              *haOrigin `json:"origin"`
}

const projectURL = "https://github.com/endreszabo/mikrotik-presence-sensor"

// Discovery builds every Home Assistant discovery message for the given
// people and polled devices.
func Discovery(t Topics, people []presence.Person, devices []string, version string) []Message {
	origin := &haOrigin{Name: "mikrotik-presence-sensor", SWVersion: version, SupportURL: projectURL}
	bridgeID := t.Node + "_bridge"
	bridge := &haDevice{
		Identifiers:  []string{bridgeID},
		Name:         "MikroTik presence bridge",
		Manufacturer: "mikrotik-presence-sensor",
		Model:        "Presence bridge",
		SWVersion:    version,
	}
	var out []Message
	add := func(component, objectID string, e entity) {
		e.Origin = origin
		b, _ := json.Marshal(e)
		out = append(out, Message{Topic: t.config(component, objectID), Payload: b})
	}

	for _, p := range people {
		dev := &haDevice{
			Identifiers:  []string{t.Node + "_person_" + p.ID},
			Name:         p.Name + " presence",
			Manufacturer: "mikrotik-presence-sensor",
			Model:        "Presence tracker",
			ViaDevice:    bridgeID,
		}
		icon := p.Icon
		if icon == "" {
			icon = "mdi:cellphone-wireless"
		}
		add("device_tracker", p.ID, entity{
			Name:                nil,
			UniqueID:            t.Node + "_" + p.ID + "_tracker",
			Icon:                icon,
			StateTopic:          t.PersonState(p.ID),
			JSONAttributesTopic: t.PersonAttributes(p.ID),
			PayloadHome:         presence.StatusHome,
			PayloadNotHome:      presence.StatusNotHome,
			SourceType:          "router",
			AvailabilityTopic:   t.Availability(),
			Device:              dev,
		})
		add("binary_sensor", p.ID+"_presence", entity{
			Name:                "Home",
			UniqueID:            t.Node + "_" + p.ID + "_presence",
			DeviceClass:         "presence",
			StateTopic:          t.PersonState(p.ID),
			JSONAttributesTopic: t.PersonAttributes(p.ID),
			PayloadOn:           presence.StatusHome,
			PayloadOff:          presence.StatusNotHome,
			AvailabilityTopic:   t.Availability(),
			Device:              dev,
		})
	}

	add("binary_sensor", "bridge_connectivity", entity{
		Name:           "Connectivity",
		UniqueID:       t.Node + "_bridge_connectivity",
		DeviceClass:    "connectivity",
		EntityCategory: "diagnostic",
		StateTopic:     t.Availability(),
		PayloadOn:      "online",
		PayloadOff:     "offline",
		Device:         bridge,
	})
	add("sensor", "bridge_people_home", entity{
		Name:              "People home",
		UniqueID:          t.Node + "_bridge_people_home",
		Icon:              "mdi:home-account",
		StateTopic:        t.BridgeState(),
		ValueTemplate:     "{{ value_json.people_home }}",
		StateClass:        "measurement",
		AvailabilityTopic: t.Availability(),
		Device:            bridge,
	})
	add("sensor", "bridge_devices_ok", entity{
		Name:              "Devices polled OK",
		UniqueID:          t.Node + "_bridge_devices_ok",
		Icon:              "mdi:router-network",
		EntityCategory:    "diagnostic",
		StateTopic:        t.BridgeState(),
		ValueTemplate:     "{{ value_json.devices_ok }}",
		AvailabilityTopic: t.Availability(),
		Device:            bridge,
	})
	for _, d := range devices {
		add("binary_sensor", "device_"+Slug(d)+"_poll", entity{
			Name:                d + " polling",
			UniqueID:            t.Node + "_device_" + Slug(d) + "_poll",
			DeviceClass:         "connectivity",
			EntityCategory:      "diagnostic",
			StateTopic:          t.DeviceState(d),
			JSONAttributesTopic: t.DeviceAttributes(d),
			PayloadOn:           "ON",
			PayloadOff:          "OFF",
			AvailabilityTopic:   t.Availability(),
			Device:              bridge,
		})
	}
	return out
}
