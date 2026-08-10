package main

import (
	"context"
	"encoding/json"
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
	"github.com/vectorcore/gmlc/internal/delivery"
	vcdiam "github.com/vectorcore/gmlc/internal/diameter"
	"github.com/vectorcore/gmlc/internal/domain"
	"github.com/vectorcore/gmlc/internal/httpapi"
	"github.com/vectorcore/gmlc/internal/logging"
	"github.com/vectorcore/gmlc/internal/lrr"
	"github.com/vectorcore/gmlc/internal/mlp"
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
		if err = st.UpsertClient(ctx, storage.Client{ID: c.ID, CredentialHash: auth.HashToken(c.BearerToken), Enabled: true, Services: c.Services, TargetPrefixes: c.TargetPrefixes, LCSClientType: c.ClientTypeValue(), LCSPrivacyCheck: c.PrivacyCheckValue()}); err != nil {
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
	purgeDone := make(chan struct{})
	purgeStop := make(chan struct{})
	go runRetentionPurge(st, cfg.Retention, purgeStop, purgeDone)
	t := vcdiam.RegistryConfig{OriginHost: cfg.Diameter.OriginHost, OriginRealm: cfg.Diameter.OriginRealm, HostIP: net.ParseIP(cfg.Diameter.HostIPAddress), ConnectTimeout: cfg.Diameter.ConnectionTimeout, ReconnectMin: cfg.Diameter.ReconnectMin, ReconnectMax: cfg.Diameter.ReconnectMax, WatchdogInterval: cfg.Diameter.WatchdogInterval, WatchdogTimeout: cfg.Diameter.WatchdogTimeout}
	for _, p := range cfg.Diameter.Peers {
		t.Peers = append(t.Peers, vcdiam.PeerConfig{Name: p.Name, Address: p.Address, Transport: p.Transport, ExpectedOriginHost: p.ExpectedOriginHost, ExpectedOriginRealm: p.ExpectedOriginRealm})
	}
	// LRR is registered before BuildRegistry, since the registry needs the
	// handler at connection-dial time. It only needs a codec-only slg.Provider
	// (no transport/registry — DecodeLRR/BuildLRA are pure marshaling), so
	// there's no ordering dependency on the registry itself.
	if cfg.LRR.Enabled {
		codec, err := slg.New(slg.Config{OriginHost: cfg.Diameter.OriginHost, OriginRealm: cfg.Diameter.OriginRealm}, nil)
		if err != nil {
			slog.Error("LRR setup failed", "error", err)
			os.Exit(1)
		}
		lrrHandler := lrr.New(codec, st)
		if cfg.MLPReporting.Enabled {
			lrrHandler.SetPusher(mlp.NewPusher(cfg.MLPReporting.StandardReportURL, cfg.MLPReporting.StandardReportClientID, cfg.MLPReporting.EmergencyReportURL, cfg.MLPReporting.EmergencyReportClientID, cfg.MLPReporting.Timeout))
		}
		t.RequestHandlers = append(t.RequestHandlers, vcdiam.RequestHandler{AppID: slg.ApplicationID, Code: slg.CommandLocationReport, Handler: lrrHandler})
	}
	registry := vcdiam.BuildRegistry(t)
	registry.Start()
	// deliveryKey is resolved once, up front, and reused both by the
	// service layer (to encrypt callback secrets at submit time — API-ASYNC)
	// and by deliveryWorker below (to decrypt them at delivery time). A GMLC
	// with delivery disabled has no key and no callback capability at all:
	// service.Submit rejects callback_url/callback_secret with
	// ErrDeliveryNotConfigured rather than accepting a promise it can't keep.
	var deliveryKey []byte
	if cfg.Delivery.Enabled {
		deliveryKey, err = cfg.Delivery.EncryptionKeyBytes()
		if err != nil {
			slog.Error("delivery configuration failed", "error", err)
			os.Exit(1)
		}
	}
	svc := service.New(st, auth.New(st))
	if deliveryKey != nil {
		svc.SetSecretEncryptor(func(secret []byte) ([]byte, error) { return delivery.EncryptSecret(deliveryKey, secret) })
	}
	resolver, err := slh.NewWithRegistry(slh.Config{OriginHost: cfg.Diameter.OriginHost, OriginRealm: cfg.Diameter.OriginRealm, DestinationRealm: cfg.Diameter.HSSRealm, DestinationHost: cfg.Diameter.HSSHost, RequestTimeout: cfg.Diameter.RequestTimeout}, registry)
	if err != nil {
		slog.Error("SLh setup failed", "error", err)
		os.Exit(1)
	}
	provider, err := slg.NewWithRegistry(slg.Config{OriginHost: cfg.Diameter.OriginHost, OriginRealm: cfg.Diameter.OriginRealm, RequestTimeout: cfg.Diameter.RequestTimeout}, registry)
	if err != nil {
		slog.Error("SLg setup failed", "error", err)
		os.Exit(1)
	}
	// The delivery worker is entirely separate from the orchestrator worker
	// below — see delivery.Worker's own doc comment for why sharing a loop
	// with arbitrary outbound HTTP callbacks would be a mistake — and is
	// only constructed at all if delivery is configured. Declared here
	// (before worker.SetCompletionHook below) so that hook's closure can
	// reference it; it's only assigned/started further down.
	var deliveryWorker *delivery.Worker
	worker := orchestrator.New(st, resolver, provider)
	worker.Start(ctx)
	svc.SetQueuedHook(worker.Notify)
	svc.SetCancelHook(worker.Cancel)
	// API-ASYNC: a request that registered a callback at submit time gets
	// its completion pushed, not just polled. This fires only for terminal
	// states the orchestrator itself drove (success or failure) — never for
	// cancellation (the caller already knows synchronously) or expiration (a
	// background sweep unrelated to any one worker iteration).
	worker.SetCompletionHook(func(r domain.Request, v domain.Result) {
		if r.SubscriptionID == nil {
			return
		}
		payload, err := json.Marshal(httpapi.RequestJSON(r, v))
		if err != nil {
			slog.Warn("callback payload marshal failed", "request_id", r.ID, "error", err)
			return
		}
		if _, err = st.CreateDelivery(context.Background(), *r.SubscriptionID, payload); err != nil {
			slog.Warn("callback delivery enqueue failed", "request_id", r.ID, "error", err)
			return
		}
		if deliveryWorker != nil {
			deliveryWorker.Notify()
		}
	})
	if cfg.Delivery.Enabled {
		deliveryWorker = delivery.New(st, delivery.Config{MaxAttempts: cfg.Delivery.MaxAttempts, RetryBackoffMin: cfg.Delivery.RetryBackoffMin, RetryBackoffMax: cfg.Delivery.RetryBackoffMax, RequestTimeout: cfg.Delivery.RequestTimeout, EncryptionKey: deliveryKey})
		deliveryWorker.Start(ctx)
	}
	// REST and MLP are two independent listeners on two different ports, not
	// mutually exclusive — either, both, or (briefly, during a supervised
	// transition) neither may run; config.Validate already rejects both
	// being disabled at once. Gate, don't delete: REST isn't being removed
	// by MLP's arrival (see docs/mlp-le-interface-plan.md).
	var server *http.Server
	if cfg.Server.Enabled == nil || *cfg.Server.Enabled {
		server = &http.Server{Addr: cfg.Server.ListenAddress, Handler: httpapi.New(svc, registry.OverallReady), ReadHeaderTimeout: 5 * time.Second}
		go func() {
			slog.Info("gmlc listening", "address", cfg.Server.ListenAddress)
			if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("http server failed", "error", err)
			}
		}()
	}
	var mlpServer *http.Server
	if cfg.MLP.Enabled {
		mlpServer = &http.Server{Addr: cfg.MLP.ListenAddress, Handler: mlp.New(svc, cfg.MLP.SyncWaitTimeout, cfg.MLP.MaxSyncWaitTimeout), ReadHeaderTimeout: 5 * time.Second}
		go func() {
			slog.Info("gmlc mlp listening", "address", cfg.MLP.ListenAddress)
			if err := mlpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("mlp http server failed", "error", err)
			}
		}()
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	stop, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()
	close(purgeStop)
	if server != nil {
		_ = server.Shutdown(stop)
	}
	if mlpServer != nil {
		_ = mlpServer.Shutdown(stop)
	}
	_ = worker.Close(stop)
	if deliveryWorker != nil {
		_ = deliveryWorker.Close(stop)
	}
	_ = registry.Close(stop)
	select {
	case <-purgeDone:
	case <-stop.Done():
	}
	_ = st.Close(stop)
}

// runRetentionPurge enforces the configured retention window on a recurring
// basis. A one-shot purge at startup is not enough: a long-running process
// would otherwise retain subscriber location data past its configured
// retention window until its next restart.
func runRetentionPurge(st storage.Store, r config.Retention, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	t := time.NewTicker(r.PurgeInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			now := time.Now().UTC()
			if err := st.Purge(context.Background(), now.Add(-r.Request), now.Add(-r.Result)); err != nil {
				slog.Warn("retention purge failed", "error", err)
			}
		}
	}
}
