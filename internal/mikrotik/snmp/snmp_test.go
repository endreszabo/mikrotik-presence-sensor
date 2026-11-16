package snmp

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gosnmp/gosnmp"

	"github.com/endreszabo/mikrotik-presence-sensor/internal/mikrotik"
)

// fakeWalker serves canned PDUs by OID prefix.
type fakeWalker struct {
	pdus  []gosnmp.SnmpPDU
	walks []string
}

func (f *fakeWalker) BulkWalkAll(root string) ([]gosnmp.SnmpPDU, error) {
	f.walks = append(f.walks, root)
	var out []gosnmp.SnmpPDU
	for _, p := range f.pdus {
		if strings.HasPrefix(p.Name, "."+root+".") {
			out = append(out, p)
		}
	}
	return out, nil
}

func pdu(oid string, v any) gosnmp.SnmpPDU { return gosnmp.SnmpPDU{Name: "." + oid, Value: v} }

func newTestClient(t *testing.T, w walker) *Client {
	t.Helper()
	c, err := New(Options{Device: "sw1", Host: "2001:db8::3", Port: 161, Version: "2c", Community: "public"})
	if err != nil {
		t.Fatal(err)
	}
	c.connect = func(context.Context) (walker, func(), error) { return w, func() {}, nil }
	c.now = func() time.Time { return time.Unix(1000, 0) }
	return c
}

func TestPoll(t *testing.T) {
	w := &fakeWalker{pdus: []gosnmp.SnmpPDU{
		pdu(oidIfName+".5", []byte("wlan1")),
		pdu(oidIfName+".7", []byte("ether3")),
		pdu(oidIfName+".9", []byte("bridge")),
		// legacy wireless: aa:bb:cc:dd:ee:01 on ifIndex 5, -61 dBm
		pdu(oidWlRtabStrength+".170.187.204.221.238.1.5", -61),
		// CAPsMAN with SSID
		pdu(oidCMRtabRxStrength+".170.187.204.221.238.2.5", -70),
		pdu(oidCMRtabSsid+".170.187.204.221.238.2.5", []byte("home")),
		// ipNetToPhysical: IPv4 192.0.2.10 and IPv6 2001:db8::10 on ifIndex 9
		pdu(oidNetToPhysAddr+".9.1.4.192.0.2.10", []byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0x01}),
		pdu(oidNetToPhysState+".9.1.4.192.0.2.10", 1),
		pdu(oidNetToPhysAddr+".9.2.16.32.1.13.184.0.0.0.0.0.0.0.0.0.0.0.16", []byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0x01}),
		pdu(oidNetToPhysState+".9.2.16.32.1.13.184.0.0.0.0.0.0.0.0.0.0.0.16", 2),
		// Q-BRIDGE FDB: one learned on port 3 (ifIndex 7), one self entry
		pdu(oidBasePortIfIndex+".3", 7),
		pdu(oidQFdbPort+".1.170.187.204.221.238.3", 3),
		pdu(oidQFdbStatus+".1.170.187.204.221.238.3", 3),
		pdu(oidQFdbPort+".1.72.143.90.0.0.1", 0),
		pdu(oidQFdbStatus+".1.72.143.90.0.0.1", 4),
	}}
	c := newTestClient(t, w)
	res := c.Poll(t.Context(), append(c.Kinds(), mikrotik.KindDHCP))

	if !errors.Is(res.Errors[mikrotik.KindDHCP], mikrotik.ErrUnsupported) {
		t.Errorf("dhcp error = %v", res.Errors[mikrotik.KindDHCP])
	}
	wl := res.Snapshots[mikrotik.KindWireless]
	if len(wl) != 1 || wl[0].MAC != "aa:bb:cc:dd:ee:01" || wl[0].Interface != "wlan1" || *wl[0].Signal != -61 {
		t.Errorf("wireless = %+v", wl)
	}
	cm := res.Snapshots[mikrotik.KindCapsman]
	if len(cm) != 1 || cm[0].MAC != "aa:bb:cc:dd:ee:02" || cm[0].SSID != "home" {
		t.Errorf("capsman = %+v", cm)
	}
	arp := res.Snapshots[mikrotik.KindARP]
	if len(arp) != 1 || arp[0].IP != "192.0.2.10" || arp[0].State != "reachable" || arp[0].Interface != "bridge" {
		t.Errorf("arp = %+v", arp)
	}
	nb := res.Snapshots[mikrotik.KindNeighbor]
	if len(nb) != 1 || nb[0].IP != "2001:db8::10" || nb[0].State != "stale" {
		t.Errorf("neighbor = %+v", nb)
	}
	fdb := res.Snapshots[mikrotik.KindBridgeHost]
	if len(fdb) != 1 || fdb[0].MAC != "aa:bb:cc:dd:ee:03" || fdb[0].Interface != "ether3" {
		t.Errorf("fdb = %+v", fdb)
	}
	walked := strings.Join(w.walks, " ")
	if strings.Count(walked, oidNetToPhysAddr) != 1 {
		t.Errorf("ipNetToPhysicalTable walked more than once: %s", walked)
	}
}

func TestFallbackTables(t *testing.T) {
	w := &fakeWalker{pdus: []gosnmp.SnmpPDU{
		pdu(oidNetToMediaAddr+".4.192.0.2.20", []byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0x04}),
		pdu(oidNetToMediaType+".4.192.0.2.20", 3),
		pdu(oidNetToMediaAddr+".4.192.0.2.21", []byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0x05}),
		pdu(oidNetToMediaType+".4.192.0.2.21", 2), // invalid
		pdu(oidFdbPort+".170.187.204.221.238.6", 2),
		pdu(oidFdbStatus+".170.187.204.221.238.6", 3),
	}}
	c := newTestClient(t, w)
	res := c.Poll(t.Context(), []mikrotik.Kind{mikrotik.KindARP, mikrotik.KindBridgeHost})
	arp := res.Snapshots[mikrotik.KindARP]
	if len(arp) != 1 || arp[0].IP != "192.0.2.20" || arp[0].State != "dynamic" || arp[0].Interface != "ifIndex4" {
		t.Errorf("arp = %+v", arp)
	}
	fdb := res.Snapshots[mikrotik.KindBridgeHost]
	if len(fdb) != 1 || fdb[0].MAC != "aa:bb:cc:dd:ee:06" || fdb[0].Interface != "port2" {
		t.Errorf("fdb = %+v", fdb)
	}
}

func TestConnectError(t *testing.T) {
	c := newTestClient(t, nil)
	c.connect = func(context.Context) (walker, func(), error) { return nil, nil, errors.New("boom") }
	res := c.Poll(t.Context(), []mikrotik.Kind{mikrotik.KindARP})
	if res.Errors[mikrotik.KindARP] == nil {
		t.Fatal("expected error")
	}
}

func TestNewValidation(t *testing.T) {
	base := Options{Host: "::1", Port: 161, Version: "3", Username: "u", SecurityLevel: "authPriv", AuthProtocol: "SHA256", PrivProtocol: "AES"}
	if _, err := New(base); err != nil {
		t.Fatalf("valid v3 options rejected: %v", err)
	}
	for name, mut := range map[string]func(*Options){
		"version":  func(o *Options) { o.Version = "1" },
		"auth":     func(o *Options) { o.AuthProtocol = "CRC32" },
		"priv":     func(o *Options) { o.PrivProtocol = "ROT13" },
		"seclevel": func(o *Options) { o.SecurityLevel = "maybe" },
	} {
		o := base
		mut(&o)
		if _, err := New(o); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
