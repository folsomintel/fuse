// fuse-proxy gives environments a stable address.
//
// it is one process with three listeners: a udp port guests dial in to
// (internal/tunnel), the public tcp ports clients connect to, and an admin api
// the orchestrator drives (internal/ingress). it holds no environment state
// beyond which public port leads to which guest, and it keeps that in a file so
// a restart brings every url back on the port it had.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/folsomintel/fuse/internal/ingress"
	"github.com/folsomintel/fuse/internal/tunnel"
)

// version is stamped at release time via -ldflags "-X main.version=...".
var version = "dev"

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func main() {
	var (
		stateDir    = flag.String("state-dir", envOr("FUSE_PROXY_STATE_DIR", "/var/lib/fuse-proxy"), "directory for routes.json and the tunnel certificate")
		publicHost  = flag.String("public-host", os.Getenv("FUSE_PROXY_PUBLIC_HOST"), "name or address clients and guests reach this machine by (required)")
		tunnelAddr  = flag.String("tunnel-listen", envOr("FUSE_PROXY_TUNNEL_LISTEN", ":7443"), "udp address guests dial in to")
		adminAddr   = flag.String("admin-listen", envOr("FUSE_PROXY_ADMIN_LISTEN", "127.0.0.1:7080"), "tcp address of the admin api")
		portRange   = flag.String("ports", envOr("FUSE_PROXY_PORTS", "20000-29999"), "public port range handed out to routes, min-max")
		bindHost    = flag.String("bind", os.Getenv("FUSE_PROXY_BIND"), "address public ports bind (default all)")
		hold        = flag.Duration("hold", 30*time.Second, "how long a client connection waits for its guest to be reachable")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(logger, *stateDir, *publicHost, *tunnelAddr, *adminAddr, *portRange, *bindHost, *hold); err != nil {
		logger.Error("fuse-proxy stopped", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger, stateDir, publicHost, tunnelAddr, adminAddr, portRange, bindHost string, hold time.Duration) error {
	// the admin token comes from the environment only: a flag would put it in
	// the process list.
	adminToken := os.Getenv("FUSE_PROXY_ADMIN_TOKEN")
	if adminToken == "" {
		return errors.New("FUSE_PROXY_ADMIN_TOKEN is required")
	}
	if publicHost == "" {
		return errors.New("-public-host (or FUSE_PROXY_PUBLIC_HOST) is required: it is the host part of every url this proxy hands out")
	}
	lo, hi, ok := strings.Cut(portRange, "-")
	portMin, errMin := strconv.Atoi(lo)
	portMax, errMax := strconv.Atoi(hi)
	if !ok || errMin != nil || errMax != nil {
		return fmt.Errorf("invalid -ports %q, want min-max", portRange)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}

	cert, err := tunnel.LoadOrCreateCert(filepath.Join(stateDir, "tunnel-cert.pem"), filepath.Join(stateDir, "tunnel-key.pem"))
	if err != nil {
		return fmt.Errorf("tunnel certificate: %w", err)
	}
	proxy, err := ingress.New(ingress.Config{
		StatePath:   filepath.Join(stateDir, "routes.json"),
		PortMin:     portMin,
		PortMax:     portMax,
		BindHost:    bindHost,
		HoldTimeout: hold,
		Logger:      logger,
	})
	if err != nil {
		return err
	}
	defer proxy.Close()

	udp, err := net.ListenPacket("udp", tunnelAddr)
	if err != nil {
		return fmt.Errorf("tunnel listener: %w", err)
	}
	defer func() { _ = udp.Close() }()
	server, err := tunnel.Listen(udp, cert, proxy.Authenticate, logger)
	if err != nil {
		return fmt.Errorf("tunnel listener: %w", err)
	}
	proxy.Start(server)

	certPEM, err := tunnel.CertPEM(cert)
	if err != nil {
		return err
	}
	admin := &http.Server{
		Addr: adminAddr,
		Handler: ingress.AdminHandler(proxy, ingress.AdminConfig{
			Token:         adminToken,
			PublicHost:    publicHost,
			TunnelPort:    udp.LocalAddr().(*net.UDPAddr).Port,
			ServerCertPEM: certPEM,
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 2)
	go func() { errCh <- server.Serve(ctx) }()
	go func() { errCh <- admin.ListenAndServe() }()
	logger.Info("fuse-proxy up", "version", version, "tunnel", udp.LocalAddr().String(), "admin", adminAddr, "ports", portRange, "public_host", publicHost)

	select {
	case <-ctx.Done():
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return admin.Shutdown(shutdownCtx)
}
