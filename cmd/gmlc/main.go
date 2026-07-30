package main

import (
	"context"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/vectorcore/gmlc/internal/auth"
	"github.com/vectorcore/gmlc/internal/config"
	vcdiam "github.com/vectorcore/gmlc/internal/diameter"
	"github.com/vectorcore/gmlc/internal/httpapi"
	"github.com/vectorcore/gmlc/internal/logging"
	"github.com/vectorcore/gmlc/internal/orchestrator"
	"github.com/vectorcore/gmlc/internal/service"
	"github.com/vectorcore/gmlc/internal/slg"
	"github.com/vectorcore/gmlc/internal/slh"
	"github.com/vectorcore/gmlc/internal/storage"
	"github.com/vectorcore/gmlc/internal/storage/sqlite"
)

var version = "dev"

func main() {
	path := flag.String("c", "config/gmlc.yaml", "configuration file")
	debug := flag.Bool("d", false, "console debug logging")
	showVersion := flag.Bool("v", false, "print version")
	flag.Parse()
	if *showVersion {
		_, _ = os.Stdout.WriteString(version + "\n")
		return
	}
	_, _ = os.Stdout.WriteString("Starting VectorCore GMLC\n")
	cfg, err := config.Load(*path)
	if err != nil {
		slog.Error("configuration failed", "error", err)
		os.Exit(1)
	}
	closeLog, err := logging.Setup(cfg.Logging.File, cfg.Logging.Level, *debug, os.Stderr)
	if err != nil {
		slog.Error("logging setup failed", "error", err)
		os.Exit(1)
	}
	defer closeLog()
	ctx := context.Background()
	st, err := sqlite.Open(ctx, sqlite.Config{Path: cfg.Database.Path, BusyTimeout: cfg.Database.BusyTimeout, Synchronous: cfg.Database.Synchronous, CheckpointPages: cfg.Database.CheckpointPages})
	if err != nil {
		slog.Error("database open failed", "error", err)
		os.Exit(1)
	}
	if err = st.Migrate(ctx); err != nil {
		slog.Error("database migration failed", "error", err)
		os.Exit(1)
	}
	for _, c := range cfg.Clients {
		if err = st.UpsertClient(ctx, storage.Client{ID: c.ID, CredentialHash: auth.HashToken(c.BearerToken), Enabled: true, Services: c.Services, TargetPrefixes: c.TargetPrefixes}); err != nil {
			slog.Error("client bootstrap failed", "client_id", c.ID, "error", err)
			os.Exit(1)
		}
	}
	if err = st.Recover(ctx, time.Now().UTC()); err != nil {
		slog.Error("recovery failed", "error", err)
		os.Exit(1)
	}
	if err = st.Purge(ctx, time.Now().UTC().Add(-cfg.Retention.Request), time.Now().UTC().Add(-cfg.Retention.Result)); err != nil {
		slog.Error("retention purge failed", "error", err)
		os.Exit(1)
	}
	t := vcdiam.RegistryConfig{OriginHost: cfg.Diameter.OriginHost, OriginRealm: cfg.Diameter.OriginRealm, HostIP: net.ParseIP(cfg.Diameter.HostIPAddress), ConnectTimeout: cfg.Diameter.ConnectionTimeout, ReconnectMin: cfg.Diameter.ReconnectMin, ReconnectMax: cfg.Diameter.ReconnectMax, WatchdogInterval: cfg.Diameter.WatchdogInterval, WatchdogTimeout: cfg.Diameter.WatchdogTimeout}
	for _, p := range cfg.Diameter.Peers {
		t.Peers = append(t.Peers, vcdiam.PeerConfig{Name: p.Name, Address: p.Address, Transport: p.Transport, ExpectedOriginHost: p.ExpectedOriginHost, ExpectedOriginRealm: p.ExpectedOriginRealm})
	}
	registry := vcdiam.BuildRegistry(t)
	registry.Start()
	svc := service.New(st, auth.New(st))
	resolver, err := slh.NewWithRegistry(slh.Config{OriginHost: cfg.Diameter.OriginHost, OriginRealm: cfg.Diameter.OriginRealm, DestinationRealm: cfg.Diameter.OriginRealm, RequestTimeout: cfg.Diameter.RequestTimeout}, registry)
	if err != nil {
		slog.Error("SLh setup failed", "error", err)
		os.Exit(1)
	}
	provider, err := slg.NewWithRegistry(slg.Config{OriginHost: cfg.Diameter.OriginHost, OriginRealm: cfg.Diameter.OriginRealm}, registry)
	if err != nil {
		slog.Error("SLg setup failed", "error", err)
		os.Exit(1)
	}
	worker := orchestrator.New(st, resolver, provider)
	worker.Start(ctx)
	svc.SetQueuedHook(worker.Notify)
	svc.SetCancelHook(worker.Cancel)
	server := &http.Server{Addr: cfg.Server.ListenAddress, Handler: httpapi.New(svc, registry.OverallReady), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		slog.Info("gmlc listening", "address", cfg.Server.ListenAddress)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("http server failed", "error", err)
		}
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	stop, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()
	_ = server.Shutdown(stop)
	_ = worker.Close(stop)
	_ = registry.Close(stop)
	_ = st.Close(stop)
}
