// Command mikrotik-presence-sensor tracks people's presence from their phones'
// MAC addresses on MikroTik access points, switches and routers, and
// publishes it to Home Assistant via MQTT discovery.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/endreszabo/mikrotik-presence-sensor/internal/config"
	"github.com/endreszabo/mikrotik-presence-sensor/internal/health"
	"github.com/endreszabo/mikrotik-presence-sensor/internal/logging"
	"github.com/endreszabo/mikrotik-presence-sensor/internal/macaddr"
	"github.com/endreszabo/mikrotik-presence-sensor/internal/metrics"
	"github.com/endreszabo/mikrotik-presence-sensor/internal/mikrotik"
	"github.com/endreszabo/mikrotik-presence-sensor/internal/mikrotik/rest"
	"github.com/endreszabo/mikrotik-presence-sensor/internal/mikrotik/snmp"
	"github.com/endreszabo/mikrotik-presence-sensor/internal/mqtt"
	"github.com/endreszabo/mikrotik-presence-sensor/internal/poller"
	"github.com/endreszabo/mikrotik-presence-sensor/internal/presence"
	"github.com/endreszabo/mikrotik-presence-sensor/internal/syslog"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck(os.Args[2:]))
	}
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("mikrotik-presence-sensor", flag.ExitOnError)
	configPath := fs.String("config", os.Getenv("MPS_CONFIG_FILE"), "path to the YAML configuration file (env MPS_CONFIG_FILE); optional when configured via MPS_* variables")
	checkOnly := fs.Bool("check-config", false, "validate the configuration, print it with secrets masked and exit")
	showVersion := fs.Bool("version", false, "print the version and exit")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: %s [flags]\n       %s healthcheck [-url URL]\n\nFlags:\n", fs.Name(), fs.Name())
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	if *showVersion {
		fmt.Println(version)
		return 0
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "configuration error:\n%v\n", err)
		return 2
	}
	if *checkOnly {
		fmt.Print(cfg.Redacted())
		return 0
	}
	logger, err := logging.New(os.Stderr, cfg.Log.Format, cfg.Log.Level)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	slog.SetDefault(logger)
	log := logging.Component(logger, "core")
	log.Info("starting", "version", version, "devices", len(cfg.Devices), "people", len(cfg.People))
	log.Debug("effective configuration", "config", cfg.Redacted())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := serve(ctx, cfg, logger); err != nil {
		log.Error("fatal error", "error", err)
		return 1
	}
	log.Info("stopped")
	return 0
}

func serve(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	log := logging.Component(logger, "core")

	var sources []mikrotik.Kind
	for _, s := range cfg.Presence.Sources {
		k, _ := mikrotik.ParseKind(s) // validated
		sources = append(sources, k)
	}
	people := make([]presence.Person, len(cfg.People))
	for i, p := range cfg.People {
		people[i] = presence.Person{ID: p.ID, Name: p.Name, Icon: p.Icon, MACs: p.MACs}
		for _, m := range p.MACs {
			if macaddr.IsLocallyAdministered(m) {
				log.Warn("MAC is locally administered (randomized/private Wi-Fi address?); make sure the phone uses a fixed address on the home network",
					"person", p.ID, "mac", m)
			}
		}
	}
	engine := presence.New(presence.Config{
		ConsiderHome:   cfg.Presence.ConsiderHome,
		EventTTL:       cfg.Presence.EventTTL,
		Sources:        sources,
		ARPStates:      cfg.Presence.ARPStates,
		NeighborStates: cfg.Presence.NeighborStates,
	}, people, presence.RealClock, logging.Component(logger, "engine"))

	pollers, err := buildPollers(cfg, sources, engine, logging.Component(logger, "poller"))
	if err != nil {
		return err
	}
	byDevice := map[string]*poller.Poller{}
	var deviceNames []string
	for _, p := range pollers {
		byDevice[p.Device()] = p
		deviceNames = append(deviceNames, p.Device())
	}
	pollerStatus := func() []poller.Status {
		out := make([]poller.Status, len(pollers))
		for i, p := range pollers {
			out[i] = p.Status()
		}
		return out
	}

	pub, err := mqtt.New(cfg.MQTT, mqtt.Sources{
		People:  people,
		Devices: deviceNames,
		States:  engine.States,
		Pollers: pollerStatus,
	}, version, logging.Component(logger, "mqtt"))
	if err != nil {
		return err
	}
	engine.OnChange(pub.PublishPerson)
	for _, p := range pollers {
		p.OnStatus(pub.PublishDevice)
	}

	var (
		met         *metrics.Metrics
		metricsHTTP http.Handler
		observer    syslog.Observer
	)
	if cfg.Health.Metrics {
		met = metrics.New(version, pub.Connected, func() int {
			n := 0
			for _, s := range engine.States() {
				if s.Home() {
					n++
				}
			}
			return n
		})
		engine.OnChange(met.PersonChanged)
		for _, p := range pollers {
			p.OnReport(met.ObservePoll)
		}
		metricsHTTP, observer = met.Handler(), met
	}

	var syslogSrv *syslog.Server
	if cfg.Syslog.Enabled {
		syslogSrv, err = buildSyslog(cfg, engine, byDevice, observer, logging.Component(logger, "syslog"))
		if err != nil {
			return err
		}
	}

	healthSrv := health.New(cfg.Health.Listen, health.Checks{
		RequireMQTT: cfg.Health.RequireMQTT,
		MQTT:        pub.Connected,
		Metrics:     metricsHTTP,
		Ready: func() bool {
			if len(pollers) == 0 {
				return true
			}
			return slices.ContainsFunc(pollerStatus(), func(s poller.Status) bool { return !s.LastSuccess.IsZero() })
		},
		Status: func() any {
			st := struct {
				Version string            `json:"version"`
				MQTT    bool              `json:"mqtt_connected"`
				People  []presence.State  `json:"people"`
				Devices []poller.Status   `json:"devices"`
				Syslog  *syslog.Stats     `json:"syslog,omitempty"`
				Time    time.Time         `json:"time"`
				Config  map[string]string `json:"config"`
			}{
				Version: version, MQTT: pub.Connected(), People: engine.States(), Devices: pollerStatus(), Time: time.Now(),
				Config: map[string]string{"consider_home": cfg.Presence.ConsiderHome.String(), "broker": cfg.MQTT.Broker},
			}
			if syslogSrv != nil {
				s := syslogSrv.Stats()
				st.Syslog = &s
			}
			return st
		},
	}, logging.Component(logger, "health"))

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return engine.Run(gctx) })
	g.Go(func() error { return pub.Run(gctx) })
	g.Go(func() error { return healthSrv.Run(gctx) })
	if syslogSrv != nil {
		g.Go(func() error { return syslogSrv.Run(gctx) })
	}
	for _, p := range pollers {
		g.Go(func() error { return p.Run(gctx) })
	}
	err = g.Wait()
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func buildPollers(cfg *config.Config, sources []mikrotik.Kind, sink poller.Sink, log *slog.Logger) ([]*poller.Poller, error) {
	var out []*poller.Poller
	for _, d := range cfg.Devices {
		if d.PollingDisabled() {
			log.Info("polling disabled, device only feeds syslog events", "device", d.Name)
			continue
		}
		var (
			client mikrotik.Client
			err    error
		)
		switch d.Transport {
		case "rest":
			client, err = rest.New(rest.Options{
				Device:             d.Name,
				Host:               d.Host,
				Port:               d.REST.Port,
				Username:           d.REST.Username,
				Password:           d.REST.Password,
				CAFile:             d.REST.CAFile,
				InsecureSkipVerify: d.REST.InsecureSkipVerify != nil && *d.REST.InsecureSkipVerify,
				ServerName:         d.REST.ServerName,
				Timeout:            d.Timeout,
			})
		case "snmp":
			client, err = snmp.New(snmp.Options{
				Device:         d.Name,
				Host:           d.Host,
				Port:           d.SNMP.Port,
				Version:        d.SNMP.Version,
				Community:      d.SNMP.Community,
				Retries:        d.SNMP.Retries,
				MaxRepetitions: d.SNMP.MaxRepetitions,
				Timeout:        d.Timeout,
				SecurityLevel:  d.SNMP.SecurityLevel,
				Username:       d.SNMP.Username,
				AuthProtocol:   d.SNMP.AuthProtocol,
				AuthPassword:   d.SNMP.AuthPassword,
				PrivProtocol:   d.SNMP.PrivProtocol,
				PrivPassword:   d.SNMP.PrivPassword,
				ContextName:    d.SNMP.ContextName,
			})
		}
		if err != nil {
			return nil, fmt.Errorf("device %q: %w", d.Name, err)
		}
		if d.REST.InsecureSkipVerify != nil && *d.REST.InsecureSkipVerify && d.Transport == "rest" {
			log.Warn("TLS certificate verification disabled", "device", d.Name)
		}

		// Only poll tables that count towards presence.
		candidates, _ := d.ParsedKinds()
		if d.AutoKinds() {
			candidates = client.Kinds()
		}
		var kinds []mikrotik.Kind
		for _, k := range candidates {
			if slices.Contains(sources, k) {
				kinds = append(kinds, k)
			} else if !d.AutoKinds() {
				log.Warn("kind is not in presence.sources, not polling it", "device", d.Name, "kind", k)
			}
		}
		if len(kinds) == 0 {
			log.Warn("nothing to poll on device", "device", d.Name)
			continue
		}
		log.Info("polling device", "device", d.Name, "host", d.Host, "transport", d.Transport,
			"kinds", kinds, "interval", d.PollInterval, "auto", d.AutoKinds())
		out = append(out, poller.New(poller.Options{
			Device:     d.Name,
			Transport:  d.Transport,
			Kinds:      kinds,
			Probe:      d.AutoKinds(),
			Interval:   d.PollInterval,
			StaleAfter: cfg.Presence.StaleAfterFailedPolls,
		}, client, sink, log))
	}
	return out, nil
}

func buildSyslog(cfg *config.Config, engine *presence.Engine, pollers map[string]*poller.Poller, observer syslog.Observer, log *slog.Logger) (*syslog.Server, error) {
	ruleCfg := slices.Clone(cfg.Syslog.Rules) // user rules take precedence
	if !cfg.Syslog.DisableDefaultRules {
		ruleCfg = append(ruleCfg, syslog.DefaultRules()...)
	}
	rules, err := syslog.CompileRules(ruleCfg)
	if err != nil {
		return nil, err
	}
	opts := syslog.Options{UDPListen: cfg.Syslog.UDPListen, TCPListen: cfg.Syslog.TCPListen, Rules: rules, Observer: observer}
	for _, d := range cfg.Devices {
		opts.Sources = append(opts.Sources, syslog.Source{Device: d.Name, Host: d.Host, Extra: d.SyslogSources})
	}
	for _, s := range cfg.Syslog.AllowedSources {
		if s == "any" {
			opts.AllowAny = true
			continue
		}
		if p, err := netip.ParsePrefix(s); err == nil {
			opts.Allowed = append(opts.Allowed, p.Masked())
			continue
		}
		a, _ := netip.ParseAddr(s) // validated
		opts.Allowed = append(opts.Allowed, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
	}
	return syslog.New(opts, func(ctx context.Context, ev presence.Event) {
		if !engine.HandleEvent(ev) {
			log.Log(ctx, slog.LevelDebug-4, "event for untracked MAC", "mac", ev.MAC, "device", ev.Device)
			return
		}
		log.InfoContext(ctx, "presence event", "event", ev.Type, "mac", ev.MAC, "device", ev.Device, "iface", ev.Interface, "kind", ev.Kind)
		// Confirm with a fresh poll: fills in signal/SSID after a join and
		// settles roaming after a leave.
		if p := pollers[ev.Device]; p != nil {
			p.Trigger()
		}
	}, log), nil
}

func healthcheck(args []string) int {
	fs := flag.NewFlagSet("healthcheck", flag.ExitOnError)
	url := fs.String("url", "", "health URL to probe (default: derived from health.listen)")
	path := fs.String("path", "/healthz", "path to probe when -url is not given")
	_ = fs.Parse(args)

	urls := []string{*url}
	if *url == "" {
		listen := config.Default().Health.Listen
		if cfg, err := config.Load(os.Getenv("MPS_CONFIG_FILE")); err == nil {
			listen = cfg.Health.Listen
		} else if l := os.Getenv("MPS_HEALTH_LISTEN"); l != "" {
			listen = l
		}
		var err error
		if urls, err = health.ProbeURLs(listen, *path); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := health.Probe(ctx, urls); err != nil {
		fmt.Fprintln(os.Stderr, "unhealthy:", err)
		return 1
	}
	return 0
}
