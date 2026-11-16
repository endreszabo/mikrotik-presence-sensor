// Package macaddr normalizes the many textual MAC address spellings seen on
// MikroTik devices and in user configuration into one canonical form.
package macaddr

import (
	"fmt"
	"net"
	"strings"
)

// Normalize parses s (aa:bb:cc:dd:ee:ff, AA-BB-CC-DD-EE-FF, aabb.ccdd.eeff)
// and returns the lower-case, colon separated canonical form.
func Normalize(s string) (string, error) {
	hw, err := net.ParseMAC(strings.TrimSpace(s))
	if err != nil {
		return "", err
	}
	if len(hw) != 6 {
		return "", fmt.Errorf("not an EUI-48 address: %q", s)
	}
	return strings.ToLower(hw.String()), nil
}

// FromBytes formats a 6 byte hardware address (as found in SNMP OctetStrings
// and OID indexes) in canonical form.
func FromBytes(b []byte) (string, bool) {
	if len(b) != 6 {
		return "", false
	}
	return strings.ToLower(net.HardwareAddr(b).String()), true
}

// IsLocallyAdministered reports whether the canonical MAC has the U/L bit set.
// Phones using per-network "private" (randomized) addresses produce such MACs.
func IsLocallyAdministered(mac string) bool {
	hw, err := net.ParseMAC(mac)
	if err != nil || len(hw) == 0 {
		return false
	}
	return hw[0]&0x02 != 0
}
