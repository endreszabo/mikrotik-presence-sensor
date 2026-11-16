// Package mikrotik defines the data model shared by the REST and SNMP
// transports: what kinds of tables can be polled and what an observation of a
// MAC address on a device looks like.
package mikrotik

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

// Kind identifies a source table (or log stream) a MAC address was seen in.
type Kind string

const (
	KindWifi       Kind = "wifi"        // /interface/wifi registration table (RouterOS 7 wifi package, incl. CAPsMAN)
	KindWireless   Kind = "wireless"    // /interface/wireless registration table (legacy package)
	KindCapsman    Kind = "capsman"     // /caps-man registration table (legacy CAPsMAN controller)
	KindARP        Kind = "arp"         // /ip/arp
	KindNeighbor   Kind = "neighbor"    // /ipv6/neighbor
	KindBridgeHost Kind = "bridge_host" // /interface/bridge/host (FDB)
	KindDHCP       Kind = "dhcp"        // /ip/dhcp-server/lease
)

// AllKinds lists every kind in preference order (most specific first).
func AllKinds() []Kind {
	return []Kind{KindWifi, KindWireless, KindCapsman, KindBridgeHost, KindNeighbor, KindARP, KindDHCP}
}

// ParseKind validates a kind name.
func ParseKind(s string) (Kind, error) {
	k := Kind(s)
	if slices.Contains(AllKinds(), k) {
		return k, nil
	}
	return "", fmt.Errorf("unknown kind %q", s)
}

// Wireless reports whether the kind is a Wi-Fi association table.
func (k Kind) Wireless() bool {
	return k == KindWifi || k == KindWireless || k == KindCapsman
}

// Rank orders kinds by how much detail and certainty they carry; lower is
// better. It decides which observation supplies a person's attributes.
func (k Kind) Rank() int {
	if i := slices.Index(AllKinds(), k); i >= 0 {
		return i
	}
	return len(AllKinds())
}

// Observation is a single sighting of a MAC address.
type Observation struct {
	MAC       string    `json:"mac"`
	Kind      Kind      `json:"kind"`
	Device    string    `json:"device"`
	Interface string    `json:"interface,omitempty"`
	SSID      string    `json:"ssid,omitempty"`
	Band      string    `json:"band,omitempty"`
	Signal    *int      `json:"signal,omitempty"`
	IP        string    `json:"ip,omitempty"`
	State     string    `json:"state,omitempty"`
	SeenAt    time.Time `json:"seen_at"`
}

// ErrUnsupported is returned (wrapped) for a kind the device or the transport
// cannot provide, e.g. the wireless package is not installed.
var ErrUnsupported = errors.New("not supported")

// Result is the outcome of one poll cycle. Kinds absent from both Snapshots
// and Errors were not requested.
type Result struct {
	Snapshots map[Kind][]Observation
	Errors    map[Kind]error
	// Durations is the time spent fetching each kind's table.
	Durations map[Kind]time.Duration
}

// NewResult returns an empty, ready to use Result.
func NewResult() Result {
	return Result{Snapshots: map[Kind][]Observation{}, Errors: map[Kind]error{}, Durations: map[Kind]time.Duration{}}
}

// Client polls a device for the requested kinds.
type Client interface {
	Poll(ctx context.Context, kinds []Kind) Result
	// Kinds returns the kinds this transport can, in principle, provide.
	Kinds() []Kind
	Close() error
}
