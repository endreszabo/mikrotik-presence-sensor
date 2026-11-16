// Package snmp polls MikroTik devices (or any standards compliant switch or
// router) over SNMP v2c/v3 using standard and MIKROTIK-MIB tables.
package snmp

import (
	"context"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/gosnmp/gosnmp"

	"github.com/endreszabo/mikrotik-presence-sensor/internal/macaddr"
	"github.com/endreszabo/mikrotik-presence-sensor/internal/mikrotik"
)

// Table column OIDs (without leading dot).
const (
	oidIfName = "1.3.6.1.2.1.31.1.1.1.1" // IF-MIB::ifName

	// MIKROTIK-MIB::mtxrWlRtabStrength, indexed by client MAC + ifIndex.
	// Populated by the legacy wireless package.
	oidWlRtabStrength = "1.3.6.1.4.1.14988.1.1.1.2.1.3"
	// MIKROTIK-MIB::mtxrWlCMRtabRxStrength / mtxrWlCMRtabSsid (legacy CAPsMAN).
	oidCMRtabRxStrength = "1.3.6.1.4.1.14988.1.1.1.5.1.11"
	oidCMRtabSsid       = "1.3.6.1.4.1.14988.1.1.1.5.1.12"

	// IP-MIB::ipNetToPhysicalPhysAddress / ipNetToPhysicalState: ARP and
	// IPv6 neighbor cache in one table, indexed by ifIndex.addrType.len.addr.
	oidNetToPhysAddr  = "1.3.6.1.2.1.4.35.1.4"
	oidNetToPhysState = "1.3.6.1.2.1.4.35.1.7"
	// IP-MIB::ipNetToMediaPhysAddress / ipNetToMediaType: legacy IPv4-only ARP.
	oidNetToMediaAddr = "1.3.6.1.2.1.4.22.1.2"
	oidNetToMediaType = "1.3.6.1.2.1.4.22.1.4"

	// BRIDGE-MIB::dot1dBasePortIfIndex maps bridge port numbers to ifIndex.
	oidBasePortIfIndex = "1.3.6.1.2.1.17.1.4.1.2"
	// Q-BRIDGE-MIB::dot1qTpFdbPort / dot1qTpFdbStatus, indexed by fdbId.mac.
	oidQFdbPort   = "1.3.6.1.2.1.17.7.1.2.2.1.2"
	oidQFdbStatus = "1.3.6.1.2.1.17.7.1.2.2.1.3"
	// BRIDGE-MIB::dot1dTpFdbPort / dot1dTpFdbStatus, indexed by mac.
	oidFdbPort   = "1.3.6.1.2.1.17.4.3.1.2"
	oidFdbStatus = "1.3.6.1.2.1.17.4.3.1.3"
)

const fdbStatusLearned = 3

var physStates = map[int64]string{1: "reachable", 2: "stale", 3: "delay", 4: "probe", 5: "invalid", 6: "unknown", 7: "incomplete"}
var mediaTypes = map[int64]string{1: "other", 2: "invalid", 3: "dynamic", 4: "permanent"}

// Options configures a Client.
type Options struct {
	Device         string
	Host           string
	Port           int
	Version        string // "2c" or "3"
	Community      string
	Retries        int
	MaxRepetitions uint32
	Timeout        time.Duration

	SecurityLevel string // noAuthNoPriv, authNoPriv, authPriv
	Username      string
	AuthProtocol  string
	AuthPassword  string
	PrivProtocol  string
	PrivPassword  string
	ContextName   string
}

type walker interface {
	BulkWalkAll(rootOid string) ([]gosnmp.SnmpPDU, error)
}

// Client polls one device. A new SNMP session is opened for every poll.
type Client struct {
	opts    Options
	connect func(ctx context.Context) (walker, func(), error)
	now     func() time.Time
}

var authProtocols = map[string]gosnmp.SnmpV3AuthProtocol{
	"MD5": gosnmp.MD5, "SHA": gosnmp.SHA, "SHA1": gosnmp.SHA, "SHA224": gosnmp.SHA224,
	"SHA256": gosnmp.SHA256, "SHA384": gosnmp.SHA384, "SHA512": gosnmp.SHA512,
}

var privProtocols = map[string]gosnmp.SnmpV3PrivProtocol{
	"DES": gosnmp.DES, "AES": gosnmp.AES, "AES128": gosnmp.AES, "AES192": gosnmp.AES192,
	"AES256": gosnmp.AES256, "AES192C": gosnmp.AES192C, "AES256C": gosnmp.AES256C,
}

// New validates options and returns a client.
func New(opts Options) (*Client, error) {
	var (
		version  gosnmp.SnmpVersion
		msgFlags gosnmp.SnmpV3MsgFlags
		usm      *gosnmp.UsmSecurityParameters
	)
	switch opts.Version {
	case "2c":
		version = gosnmp.Version2c
	case "3":
		version = gosnmp.Version3
		usm = &gosnmp.UsmSecurityParameters{UserName: opts.Username}
		switch strings.ToLower(opts.SecurityLevel) {
		case "noauthnopriv":
			msgFlags = gosnmp.NoAuthNoPriv
		case "authnopriv", "authpriv":
			ap, ok := authProtocols[strings.ToUpper(opts.AuthProtocol)]
			if !ok {
				return nil, fmt.Errorf("unknown SNMPv3 auth protocol %q", opts.AuthProtocol)
			}
			usm.AuthenticationProtocol, usm.AuthenticationPassphrase = ap, opts.AuthPassword
			msgFlags = gosnmp.AuthNoPriv
			if strings.EqualFold(opts.SecurityLevel, "authpriv") {
				pp, ok := privProtocols[strings.ToUpper(opts.PrivProtocol)]
				if !ok {
					return nil, fmt.Errorf("unknown SNMPv3 privacy protocol %q", opts.PrivProtocol)
				}
				usm.PrivacyProtocol, usm.PrivacyPassphrase = pp, opts.PrivPassword
				msgFlags = gosnmp.AuthPriv
			}
		default:
			return nil, fmt.Errorf("unknown SNMPv3 security level %q", opts.SecurityLevel)
		}
	default:
		return nil, fmt.Errorf("unsupported SNMP version %q", opts.Version)
	}
	c := &Client{opts: opts, now: time.Now}
	c.connect = func(ctx context.Context) (walker, func(), error) {
		// A fresh session (engine boots, request IDs) per poll.
		g := &gosnmp.GoSNMP{
			Context:        ctx,
			Target:         opts.Host,
			Port:           uint16(opts.Port),
			Transport:      "udp",
			Version:        version,
			Community:      opts.Community,
			Timeout:        opts.Timeout,
			Retries:        opts.Retries,
			MaxRepetitions: opts.MaxRepetitions,
			MaxOids:        gosnmp.MaxOids,
		}
		if usm != nil {
			g.SecurityModel = gosnmp.UserSecurityModel
			g.MsgFlags = msgFlags
			g.ContextName = opts.ContextName
			g.SecurityParameters = usm.Copy()
		}
		if err := g.Connect(); err != nil {
			return nil, nil, err
		}
		return g, func() { _ = g.Conn.Close() }, nil
	}
	return c, nil
}

// Kinds implements mikrotik.Client. The new wifi package and the legacy
// wireless package share the MIKROTIK-MIB registration table, so only
// "wireless" is probed automatically; "wifi" may be requested explicitly.
func (c *Client) Kinds() []mikrotik.Kind {
	return []mikrotik.Kind{mikrotik.KindWireless, mikrotik.KindCapsman, mikrotik.KindBridgeHost, mikrotik.KindNeighbor, mikrotik.KindARP}
}

// Close implements mikrotik.Client.
func (c *Client) Close() error { return nil }

// Poll implements mikrotik.Client.
func (c *Client) Poll(ctx context.Context, kinds []mikrotik.Kind) mikrotik.Result {
	res := mikrotik.NewResult()
	w, closeFn, err := c.connect(ctx)
	if err != nil {
		for _, k := range kinds {
			res.Errors[k] = fmt.Errorf("snmp connect: %w", err)
		}
		return res
	}
	defer closeFn()
	p := &poll{c: c, w: w, now: c.now()}
	for _, k := range kinds {
		var obs []mikrotik.Observation
		start := time.Now()
		switch k {
		case mikrotik.KindWifi, mikrotik.KindWireless:
			obs, err = p.registrations(k)
		case mikrotik.KindCapsman:
			obs, err = p.capsman()
		case mikrotik.KindARP, mikrotik.KindNeighbor:
			obs, err = p.neighbors(k)
		case mikrotik.KindBridgeHost:
			obs, err = p.fdb()
		default:
			err = fmt.Errorf("%s: %w by the SNMP transport", k, mikrotik.ErrUnsupported)
		}
		res.Durations[k] = time.Since(start)
		if err != nil {
			res.Errors[k] = err
			continue
		}
		res.Snapshots[k] = obs
	}
	return res
}

// poll caches tables shared by several kinds within one poll cycle.
type poll struct {
	c       *Client
	w       walker
	now     time.Time
	ifNames map[int]string
	netPhys []mikrotik.Observation
	netDone bool
}

func (p *poll) walk(oid string) ([]gosnmp.SnmpPDU, error) {
	pdus, err := p.w.BulkWalkAll(oid)
	if err != nil {
		return nil, fmt.Errorf("walking %s: %w", oid, err)
	}
	return pdus, nil
}

func (p *poll) ifName(idx int) string {
	if p.ifNames == nil {
		p.ifNames = map[int]string{}
		if pdus, err := p.walk(oidIfName); err == nil {
			for _, pdu := range pdus {
				if ix, ok := suffix(pdu.Name, oidIfName); ok && len(ix) == 1 {
					p.ifNames[ix[0]] = str(pdu.Value)
				}
			}
		}
	}
	if n, ok := p.ifNames[idx]; ok {
		return n
	}
	return "ifIndex" + strconv.Itoa(idx)
}

func (p *poll) obs(kind mikrotik.Kind, mac string) mikrotik.Observation {
	return mikrotik.Observation{MAC: mac, Kind: kind, Device: p.c.opts.Device, SeenAt: p.now}
}

func (p *poll) registrations(kind mikrotik.Kind) ([]mikrotik.Observation, error) {
	pdus, err := p.walk(oidWlRtabStrength)
	if err != nil {
		return nil, err
	}
	var out []mikrotik.Observation
	for _, pdu := range pdus {
		ix, ok := suffix(pdu.Name, oidWlRtabStrength)
		if !ok || len(ix) != 7 {
			continue
		}
		mac, ok := macFromIndex(ix[:6])
		if !ok {
			continue
		}
		o := p.obs(kind, mac)
		o.Interface = p.ifName(ix[6])
		sig := int(gosnmp.ToBigInt(pdu.Value).Int64())
		o.Signal = &sig
		out = append(out, o)
	}
	return out, nil
}

func (p *poll) capsman() ([]mikrotik.Observation, error) {
	sig, err := p.walk(oidCMRtabRxStrength)
	if err != nil {
		return nil, err
	}
	ssids := map[string]string{}
	if pdus, err := p.walk(oidCMRtabSsid); err == nil {
		for _, pdu := range pdus {
			if ix, ok := suffix(pdu.Name, oidCMRtabSsid); ok {
				ssids[joinIndex(ix)] = str(pdu.Value)
			}
		}
	}
	var out []mikrotik.Observation
	for _, pdu := range sig {
		ix, ok := suffix(pdu.Name, oidCMRtabRxStrength)
		if !ok || len(ix) != 7 {
			continue
		}
		mac, ok := macFromIndex(ix[:6])
		if !ok {
			continue
		}
		o := p.obs(mikrotik.KindCapsman, mac)
		o.Interface = p.ifName(ix[6])
		o.SSID = ssids[joinIndex(ix)]
		s := int(gosnmp.ToBigInt(pdu.Value).Int64())
		o.Signal = &s
		out = append(out, o)
	}
	return out, nil
}

// neighbors returns ARP (IPv4) or neighbor cache (IPv6) entries from
// ipNetToPhysicalTable, falling back to ipNetToMediaTable for IPv4.
func (p *poll) neighbors(kind mikrotik.Kind) ([]mikrotik.Observation, error) {
	if !p.netDone {
		if err := p.loadNetToPhysical(); err != nil {
			return nil, err
		}
		p.netDone = true
	}
	var out []mikrotik.Observation
	for _, o := range p.netPhys {
		if o.Kind == kind {
			out = append(out, o)
		}
	}
	if kind == mikrotik.KindARP && len(out) == 0 {
		return p.netToMedia()
	}
	return out, nil
}

func (p *poll) loadNetToPhysical() error {
	addrs, err := p.walk(oidNetToPhysAddr)
	if err != nil {
		return err
	}
	states := map[string]string{}
	if pdus, err := p.walk(oidNetToPhysState); err == nil {
		for _, pdu := range pdus {
			if ix, ok := suffix(pdu.Name, oidNetToPhysState); ok {
				states[joinIndex(ix)] = physStates[gosnmp.ToBigInt(pdu.Value).Int64()]
			}
		}
	}
	for _, pdu := range addrs {
		ix, ok := suffix(pdu.Name, oidNetToPhysAddr)
		// ifIndex . addrType . length . address octets
		if !ok || len(ix) < 4 || ix[2] != len(ix)-3 {
			continue
		}
		mac, ok := macaddr.FromBytes(octets(pdu.Value))
		if !ok {
			continue
		}
		raw := make([]byte, ix[2])
		for i := range raw {
			raw[i] = byte(ix[3+i])
		}
		ip, ok := netip.AddrFromSlice(raw)
		if !ok {
			continue
		}
		kind := mikrotik.KindARP
		if ix[1] == 2 || ix[1] == 4 { // ipv6, ipv6z
			kind = mikrotik.KindNeighbor
		}
		o := p.obs(kind, mac)
		o.Interface = p.ifName(ix[0])
		o.IP = ip.String()
		o.State = states[joinIndex(ix)]
		p.netPhys = append(p.netPhys, o)
	}
	return nil
}

func (p *poll) netToMedia() ([]mikrotik.Observation, error) {
	addrs, err := p.walk(oidNetToMediaAddr)
	if err != nil {
		return nil, err
	}
	types := map[string]string{}
	if pdus, err := p.walk(oidNetToMediaType); err == nil {
		for _, pdu := range pdus {
			if ix, ok := suffix(pdu.Name, oidNetToMediaType); ok {
				types[joinIndex(ix)] = mediaTypes[gosnmp.ToBigInt(pdu.Value).Int64()]
			}
		}
	}
	var out []mikrotik.Observation
	for _, pdu := range addrs {
		ix, ok := suffix(pdu.Name, oidNetToMediaAddr)
		if !ok || len(ix) != 5 {
			continue
		}
		mac, ok := macaddr.FromBytes(octets(pdu.Value))
		if !ok {
			continue
		}
		state := types[joinIndex(ix)]
		if state == "invalid" {
			continue
		}
		o := p.obs(mikrotik.KindARP, mac)
		o.Interface = p.ifName(ix[0])
		o.IP = fmt.Sprintf("%d.%d.%d.%d", ix[1], ix[2], ix[3], ix[4])
		o.State = state
		out = append(out, o)
	}
	return out, nil
}

// fdb reads learned bridge host entries from Q-BRIDGE-MIB, falling back to
// the VLAN-unaware BRIDGE-MIB table.
func (p *poll) fdb() ([]mikrotik.Observation, error) {
	portIf := map[int]int{}
	if pdus, err := p.walk(oidBasePortIfIndex); err == nil {
		for _, pdu := range pdus {
			if ix, ok := suffix(pdu.Name, oidBasePortIfIndex); ok && len(ix) == 1 {
				portIf[ix[0]] = int(gosnmp.ToBigInt(pdu.Value).Int64())
			}
		}
	}
	read := func(portOID, statusOID string, macOffset int) ([]mikrotik.Observation, error) {
		ports, err := p.walk(portOID)
		if err != nil {
			return nil, err
		}
		status := map[string]int64{}
		if pdus, err := p.walk(statusOID); err == nil {
			for _, pdu := range pdus {
				if ix, ok := suffix(pdu.Name, statusOID); ok {
					status[joinIndex(ix)] = gosnmp.ToBigInt(pdu.Value).Int64()
				}
			}
		}
		seen := map[string]bool{}
		var out []mikrotik.Observation
		for _, pdu := range ports {
			ix, ok := suffix(pdu.Name, portOID)
			if !ok || len(ix) != macOffset+6 {
				continue
			}
			if st, ok := status[joinIndex(ix)]; ok && st != fdbStatusLearned {
				continue
			}
			mac, ok := macFromIndex(ix[macOffset:])
			if !ok || seen[mac] {
				continue
			}
			seen[mac] = true
			o := p.obs(mikrotik.KindBridgeHost, mac)
			port := int(gosnmp.ToBigInt(pdu.Value).Int64())
			if ifIdx, ok := portIf[port]; ok {
				o.Interface = p.ifName(ifIdx)
			} else {
				o.Interface = "port" + strconv.Itoa(port)
			}
			out = append(out, o)
		}
		return out, nil
	}
	out, err := read(oidQFdbPort, oidQFdbStatus, 1)
	if err != nil || len(out) > 0 {
		return out, err
	}
	return read(oidFdbPort, oidFdbStatus, 0)
}

// suffix returns the numeric index following the column OID.
func suffix(name, column string) ([]int, bool) {
	name = strings.TrimPrefix(name, ".")
	rest, ok := strings.CutPrefix(name, column+".")
	if !ok {
		return nil, false
	}
	parts := strings.Split(rest, ".")
	ix := make([]int, len(parts))
	for i, s := range parts {
		n, err := strconv.Atoi(s)
		if err != nil {
			return nil, false
		}
		ix[i] = n
	}
	return ix, true
}

func joinIndex(ix []int) string {
	s := make([]string, len(ix))
	for i, n := range ix {
		s[i] = strconv.Itoa(n)
	}
	return strings.Join(s, ".")
}

func macFromIndex(ix []int) (string, bool) {
	if len(ix) != 6 {
		return "", false
	}
	b := make([]byte, 6)
	for i, n := range ix {
		if n < 0 || n > 255 {
			return "", false
		}
		b[i] = byte(n)
	}
	return macaddr.FromBytes(b)
}

func octets(v any) []byte {
	switch b := v.(type) {
	case []byte:
		return b
	case string:
		return []byte(b)
	}
	return nil
}

func str(v any) string { return string(octets(v)) }
