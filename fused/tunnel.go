package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/folsomintel/fuse/internal/tunnel"
)

// runTunnel is `fused tunnel`: the sidecar that keeps this guest reachable
// through fuse-proxy.
//
// it ships inside the agent's binary so there is nothing extra to bake into a
// rootfs or download, but it runs as a unit of its own (fuse-tunnel.service,
// written by the host agent). the agent is restarted whenever its credentials
// change, on every fork and migrate, and the connections this process carries
// for the guest's other services must not die with it.
func runTunnel(args []string) int {
	fs := flag.NewFlagSet("tunnel", flag.ExitOnError)
	configPath := fs.String("config", "/fuse/tunnel.json", "path to the tunnel config written by the orchestrator")
	_ = fs.Parse(args)

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	raw, err := os.ReadFile(*configPath)
	if err != nil {
		logger.Error("read tunnel config", "err", err)
		return 1
	}
	var cfg tunnel.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		logger.Error("parse tunnel config", "path", *configPath, "err", err)
		return 1
	}
	if cfg.ProxyAddr == "" || cfg.Owner == "" || cfg.Token == "" || cfg.ServerCertPEM == "" {
		logger.Error(fmt.Sprintf("%s is missing proxy_addr, owner, token or server_cert_pem", *configPath))
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	_ = tunnel.Run(ctx, cfg, logger)
	return 0
}
