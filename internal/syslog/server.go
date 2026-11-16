package syslog

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/endreszabo/zorplogger/session"

	"github.com/endreszabo/mikrotik-presence-sensor/internal/presence"
)

const (
	maxMessage   = 64 << 10
	maxTCPConns  = 64
	tcpIdle      = 30 * time.Minute
	refreshEvery = 5 * time.Minute
)

// Source is a device allowed to send logs; Host is resolved periodically.
type Source struct {
	Device string
	Host   string
	Extra  []string
}

// Handler receives events attributed to a device.
type Handler func(ctx context.Context, ev presence.Event)

// Observer is notified about every received message, for metrics.
type Observer interface {
	// Message is called for each message from an allowed sender; m is nil
	// when no rule matched.
	Message(device string, m *Match)
	// Dropped is called for each message from an unauthorized sender.
	Dropped()
}

// Options configures a Server.
type Options struct {
	UDPListen string
	TCPListen string
	Rules     []Rule
	Sources   []Source
	// Allowed are extra prefixes; AllowAny disables source checks.
	Allowed  []netip.Prefix
	AllowAny bool
	Observer Observer // optional
}

// Stats are message counters.
type Stats struct {
	Received uint64 `json:"received"`
	Matched  uint64 `json:"matched"`
	Dropped  uint64 `json:"dropped_unauthorized"`
}

// Server receives syslog over UDP and TCP.
type Server struct {
	opts    Options
	handler Handler
	log     *slog.Logger

	mu      sync.RWMutex
	devices map[netip.Addr]string

	received, matched, dropped atomic.Uint64
	ready                      chan struct{}
	udpAddr, tcpAddr           net.Addr
}

// New creates a server; call Run to start listening.
func New(opts Options, handler Handler, log *slog.Logger) *Server {
	return &Server{opts: opts, handler: handler, log: log, devices: map[netip.Addr]string{}, ready: make(chan struct{})}
}

// Stats returns the counters.
func (s *Server) Stats() Stats {
	return Stats{Received: s.received.Load(), Matched: s.matched.Load(), Dropped: s.dropped.Load()}
}

// Run listens until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	s.refresh(ctx)
	var lc net.ListenConfig
	var wg sync.WaitGroup
	errc := make(chan error, 2)

	if s.opts.UDPListen != "" {
		pc, err := lc.ListenPacket(ctx, "udp", s.opts.UDPListen)
		if err != nil {
			return fmt.Errorf("syslog udp: %w", err)
		}
		s.udpAddr = pc.LocalAddr()
		s.log.Info("listening for syslog", "proto", "udp", "addr", s.udpAddr)
		wg.Go(func() { errc <- s.serveUDP(ctx, pc) })
		context.AfterFunc(ctx, func() { _ = pc.Close() })
	}
	if s.opts.TCPListen != "" {
		ln, err := lc.Listen(ctx, "tcp", s.opts.TCPListen)
		if err != nil {
			return fmt.Errorf("syslog tcp: %w", err)
		}
		s.tcpAddr = ln.Addr()
		s.log.Info("listening for syslog", "proto", "tcp", "addr", s.tcpAddr)
		wg.Go(func() { errc <- s.serveTCP(ctx, ln) })
		context.AfterFunc(ctx, func() { _ = ln.Close() })
	}
	close(s.ready)

	wg.Go(func() {
		t := time.NewTicker(refreshEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.refresh(ctx)
			}
		}
	})
	wg.Wait()
	close(errc)
	for err := range errc {
		if err != nil {
			return err
		}
	}
	return nil
}

// refresh resolves device hosts to addresses for attribution and allowlisting.
// When several devices share an address, the first configured one wins.
func (s *Server) refresh(ctx context.Context) {
	m := map[netip.Addr]string{}
	add := func(a netip.Addr, dev string) {
		if _, taken := m[a.Unmap()]; !taken {
			m[a.Unmap()] = dev
		}
	}
	for _, src := range s.opts.Sources {
		if a, err := netip.ParseAddr(src.Host); err == nil {
			add(a, src.Device)
		} else {
			rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			addrs, err := net.DefaultResolver.LookupNetIP(rctx, "ip", src.Host)
			cancel()
			if err != nil {
				s.log.Warn("cannot resolve device for syslog attribution", "device", src.Device, "host", src.Host, "error", err)
			}
			for _, a := range addrs {
				add(a, src.Device)
			}
		}
		for _, e := range src.Extra {
			if a, err := netip.ParseAddr(e); err == nil {
				add(a, src.Device)
			}
		}
	}
	s.mu.Lock()
	s.devices = m
	s.mu.Unlock()
}

// attribute maps a source address to a device name; ok is false when the
// sender is not allowed.
func (s *Server) attribute(addr netip.Addr) (string, bool) {
	addr = addr.Unmap()
	s.mu.RLock()
	dev, known := s.devices[addr]
	s.mu.RUnlock()
	if known {
		return dev, true
	}
	if s.opts.AllowAny {
		return addr.String(), true
	}
	for _, p := range s.opts.Allowed {
		if p.Contains(addr) {
			return addr.String(), true
		}
	}
	return "", false
}

func (s *Server) serveUDP(ctx context.Context, pc net.PacketConn) error {
	buf := make([]byte, maxMessage)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			s.log.Warn("udp read failed", "error", err)
			continue
		}
		ap, ok := addrPort(from)
		if !ok {
			continue
		}
		dev, ok := s.attribute(ap.Addr())
		if !ok {
			s.reject(ap.Addr())
			continue
		}
		for line := range bytes.SplitSeq(buf[:n], []byte{'\n'}) {
			if len(bytes.TrimSpace(line)) > 0 {
				s.handleLine(ctx, dev, string(line))
			}
		}
	}
}

func (s *Server) serveTCP(ctx context.Context, ln net.Listener) error {
	sem := make(chan struct{}, maxTCPConns)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			s.log.Warn("tcp accept failed", "error", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		ap, _ := addrPort(conn.RemoteAddr())
		dev, ok := s.attribute(ap.Addr())
		if !ok {
			s.reject(ap.Addr())
			_ = conn.Close()
			continue
		}
		select {
		case sem <- struct{}{}:
		default:
			s.log.Warn("too many syslog connections, refusing", "from", ap)
			_ = conn.Close()
			continue
		}
		wg.Go(func() {
			defer func() { <-sem }()
			s.serveConn(session.NewContext(ctx), conn, dev)
		})
	}
}

func (s *Server) serveConn(ctx context.Context, conn net.Conn, dev string) {
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	log := s.log.With("device", dev, "from", conn.RemoteAddr().String())
	log.InfoContext(ctx, "syslog connection opened")
	r := bufio.NewReaderSize(conn, maxMessage)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(tcpIdle))
		msg, err := readFrame(r)
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				log.WarnContext(ctx, "syslog connection error", "error", err)
			}
			log.InfoContext(ctx, "syslog connection closed")
			return
		}
		if strings.TrimSpace(msg) != "" {
			s.handleLine(ctx, dev, msg)
		}
	}
}

// readFrame reads one RFC 6587 frame: octet-counted ("LEN SP MSG") when the
// frame starts with a digit, newline (or NUL) delimited otherwise.
func readFrame(r *bufio.Reader) (string, error) {
	b, err := r.Peek(1)
	if err != nil {
		return "", err
	}
	if b[0] >= '1' && b[0] <= '9' {
		lenStr, err := r.ReadString(' ')
		if err != nil {
			return "", err
		}
		n, err := strconv.Atoi(strings.TrimSpace(lenStr))
		if err != nil || n <= 0 || n > maxMessage {
			return "", fmt.Errorf("invalid octet count %q", lenStr)
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", err
		}
		return string(buf), nil
	}
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(line) < maxMessage {
			line = append(line, chunk[:min(len(chunk), maxMessage-len(line))]...)
		}
		switch {
		case err == nil:
			return strings.TrimRight(string(line), "\r\n\x00"), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue // over-long line: keep the head, skip the rest
		case errors.Is(err, io.EOF) && len(line) > 0:
			return strings.TrimRight(string(line), "\r\n\x00"), nil
		default:
			return "", err
		}
	}
}

func (s *Server) reject(addr netip.Addr) {
	if s.opts.Observer != nil {
		s.opts.Observer.Dropped()
	}
	if s.dropped.Add(1)%100 == 1 { // rate limited
		s.log.Warn("dropping syslog from unauthorized source (add it to syslog.allowed_sources or a device's syslog_sources)", "from", addr, "dropped_total", s.dropped.Load())
	}
}

func (s *Server) handleLine(ctx context.Context, dev, line string) {
	s.received.Add(1)
	text := StripHeader(line)
	m, ok := MatchLine(s.opts.Rules, text)
	if !ok {
		if s.opts.Observer != nil {
			s.opts.Observer.Message(dev, nil)
		}
		s.log.Log(ctx, slog.LevelDebug-4, "syslog line ignored", "device", dev, "text", text)
		return
	}
	s.matched.Add(1)
	if s.opts.Observer != nil {
		s.opts.Observer.Message(dev, &m)
	}
	s.log.DebugContext(ctx, "syslog event", "device", dev, "rule", m.Rule, "event", m.Event, "mac", m.MAC, "iface", m.Interface)
	s.handler(ctx, presence.Event{Type: m.Event, MAC: m.MAC, Kind: m.Kind, Device: dev, Interface: m.Interface, IP: m.IP})
}

func addrPort(a net.Addr) (netip.AddrPort, bool) {
	switch v := a.(type) {
	case *net.UDPAddr:
		return v.AddrPort(), true
	case *net.TCPAddr:
		return v.AddrPort(), true
	}
	ap, err := netip.ParseAddrPort(a.String())
	return ap, err == nil
}
