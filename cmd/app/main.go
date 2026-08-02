// Command location-agent materialises Runtime resources as containers on the
// machine it runs on.
//
// It is the laptop counterpart of the platform's Kubernetes controller: same
// resources, same control plane, a different way of making them real. Which
// runtimes belong here is decided by the shard — the location's name.
package main

import (
	"context"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	rtm "github.com/reconcile-kit/runtime-manager"

	platform "scm.x5.ru/dis.cloud/core/agent-platform-cloop/api"
	pa "scm.x5.ru/dis.cloud/core/provision-agent/api"
	runtimectrl "scm.x5.ru/dis.cloud/core/location-agent/internal/controllers/runtime"
	"scm.x5.ru/dis.cloud/core/location-agent/internal/config"
	"scm.x5.ru/dis.cloud/core/location-agent/internal/repository/container"
	"scm.x5.ru/dis.cloud/core/location-agent/internal/repository/storage"
	provisionsvc "scm.x5.ru/dis.cloud/core/location-agent/internal/services/provision"
	runtimesvc "scm.x5.ru/dis.cloud/core/location-agent/internal/services/runtime"
	"scm.x5.ru/dis.cloud/core/location-agent/pkg/logger"
)

var Version string

// shutdownGrace bounds how long a clean stop may take.
const shutdownGrace = 10 * time.Second

func main() {
	slogger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	wrapLogger := &logger.LoggerWrap{Logger: slogger}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("configuration: %v", err)
	}

	// One agent per location. Two would take turns destroying each other's
	// containers, and the symptom looks nothing like the cause.
	lock, err := config.Acquire(cfg.ShardID)
	if err != nil {
		log.Fatalf("%v", err)
	}
	defer lock.Release()

	slogger.Info("starting location-agent",
		"version", Version,
		"location", cfg.ShardID,
		"bind", cfg.BindAddress,
		"ports", intRange(cfg.PortMin, cfg.PortMax),
		"dryRun", cfg.DryRun,
	)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	mgr := startManager(slogger, wrapLogger, cfg)

	// runtime-manager Run() is non-blocking — listeners and reconcilers run in
	// goroutines.
	if err := mgr.Run(ctx); err != nil {
		slogger.Error("manager run failed", "error", err)
		os.Exit(1)
	}

	slogger.Info("location-agent running, awaiting shutdown signal")
	<-ctx.Done()
	slogger.Info("shutdown signal received, stopping")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer shutdownCancel()

	// Shutdown is given a deadline of its own, and missing it is not allowed to
	// keep the process alive: the lock on the location is held by this process,
	// so an agent that will not die also blocks its replacement — with a
	// message that looks like the operator started two by mistake.
	done := make(chan error, 1)
	go func() { done <- mgr.Shutdown(shutdownCtx) }()

	select {
	case err := <-done:
		if err != nil {
			slogger.Error("manager shutdown error", "error", err)
		}
		slogger.Info("location-agent stopped")
	case <-time.After(shutdownGrace):
		slogger.Error("shutdown did not finish in time, exiting anyway", "grace", shutdownGrace.String())
		os.Exit(1)
	}
}

func startManager(slogger *slog.Logger, wrapLogger *logger.LoggerWrap, cfg config.Config) *rtm.Manager {
	opts := []rtm.Option{rtm.WithLogger(wrapLogger)}
	if cfg.InformerUsername != "" || cfg.InformerPassword != "" || cfg.InformerTLS {
		opts = append(opts, rtm.WithInformerAuthConfig(&rtm.InformerAuthConfig{
			Username:  cfg.InformerUsername,
			Password:  cfg.InformerPassword,
			EnableTLS: cfg.InformerTLS,
		}))
	}

	mgr := rtm.New(cfg.ShardID, cfg.InformerURL, cfg.StateManagerURL, opts...)

	// Templates and user configs live in shared namespaces owned by nobody's
	// shard, so they are read through remote clients rather than watched.
	if err := rtm.SetRemoteClient[*platform.RuntimeTemplate](mgr); err != nil {
		log.Fatalf("register RuntimeTemplate remote client: %v", err)
	}
	if err := rtm.SetRemoteClient[*pa.UserConfig](mgr); err != nil {
		log.Fatalf("register UserConfig remote client: %v", err)
	}
	// Provisions belong to each runtime's own shard, not to ours, so they are
	// written through a remote client rather than the watched storage.
	if err := rtm.SetRemoteClient[*pa.Provision](mgr); err != nil {
		log.Fatalf("register Provision remote client: %v", err)
	}

	resources := storage.New()

	var containers runtimesvc.ContainerRepository = container.NewDocker(cfg.DockerBinary, cfg.BindAddress, slogger)
	if cfg.DryRun {
		containers = container.NewDryRun(slogger)
	}

	provisionSvc := provisionsvc.New(resources, slogger)

	svc := runtimesvc.New(runtimesvc.Config{
		Location:     cfg.ShardID,
		BindAddress:  cfg.BindAddress,
		ExternalHost: cfg.ExternalHost,
		PortMin:      cfg.PortMin,
		PortMax:      cfg.PortMax,
		AgentEnv:     agentEnv(cfg),
	}, resources, containers, provisionSvc, slogger)

	if err := rtm.SetController[*platform.Runtime](mgr,
		runtimectrl.NewReconciler[*platform.Runtime](slogger, svc, resources),
	); err != nil {
		log.Fatalf("register Runtime controller: %v", err)
	}

	return mgr
}

// agentEnv is what every runtime's own agent needs to reach the same control
// plane this one talks to. Passing our own settings down means a runtime is
// never configured against a different platform than the one that created it.
func agentEnv(cfg config.Config) map[string]string {
	env := map[string]string{
		"STATE_MANAGER_URL": cfg.StateManagerURL,
		"INFORMER_URL":      cfg.InformerURL,
		"HTTP_ADDR":         ":8080",
	}
	if cfg.InformerUsername != "" {
		env["INFORMER_USERNAME"] = cfg.InformerUsername
	}
	if cfg.InformerPassword != "" {
		env["INFORMER_PASSWORD"] = cfg.InformerPassword
	}
	if cfg.InformerTLS {
		env["INFORMER_ENABLE_TLS"] = "true"
	}
	return env
}

func intRange(min, max int) string {
	return itoa(min) + "-" + itoa(max)
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
