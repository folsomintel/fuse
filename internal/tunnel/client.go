package tunnel

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"time"

	"github.com/quic-go/quic-go"
)

// Run is the sidecar: it keeps one connection to the proxy up for as long as
// ctx lives, and connects every stream the proxy opens to a loopback port.
//
// it returns only when ctx is done. everything else is retried, because the
// process this runs in is the thing that has to outlive fused restarts,
// network outages and the guest being moved.
func Run(ctx context.Context, cfg Config, logger *slog.Logger) error {
	backoff := time.Second
	for {
		started := time.Now()
		err := runOnce(ctx, cfg, logger)
		if ctx.Err() != nil {
			return nil
		}
		logger.Warn("tunnel down, reconnecting", "err", err, "in", backoff)
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 15*time.Second)
	}
}

func runOnce(ctx context.Context, cfg Config, logger *slog.Logger) error {
	qc := quicConfig()
	qc.KeepAlivePeriod = keepAlive
	conn, err := quic.DialAddr(ctx, cfg.ProxyAddr, pinnedTLS(cfg.ServerCertSHA256), qc)
	if err != nil {
		return err
	}
	defer func() { _ = conn.CloseWithError(0, "sidecar stopping") }()

	hctx, cancel := context.WithTimeout(ctx, helloTimeout)
	defer cancel()
	control, err := conn.OpenStreamSync(hctx)
	if err != nil {
		return err
	}
	_ = control.SetDeadline(time.Now().Add(helloTimeout))
	if err := writeJSONLine(control, hello{Owner: cfg.Owner, Token: cfg.Token}); err != nil {
		return err
	}
	var reply helloReply
	if err := readJSONLine(bufio.NewReaderSize(control, 4096), &reply); err != nil {
		return err
	}
	if reply.Error != "" {
		return fmt.Errorf("proxy refused the tunnel: %s", reply.Error)
	}
	_ = control.SetDeadline(time.Time{})
	logger.Info("tunnel up", "proxy", cfg.ProxyAddr, "owner", cfg.Owner)

	for {
		stream, err := conn.AcceptStream(ctx)
		if err != nil {
			return err
		}
		go serveStream(conn, stream, cfg.Ports, logger)
	}
}

func serveStream(conn *quic.Conn, stream *quic.Stream, ports []int, logger *slog.Logger) {
	r := bufio.NewReaderSize(stream, 32<<10)
	_ = stream.SetReadDeadline(time.Now().Add(helloTimeout))
	var h streamHeader
	if err := readJSONLine(r, &h); err != nil {
		stream.CancelRead(0)
		stream.CancelWrite(0)
		return
	}
	_ = stream.SetReadDeadline(time.Time{})
	sc := &streamConn{Stream: stream, r: r, local: conn.LocalAddr(), remote: conn.RemoteAddr()}

	if h.Port == 0 {
		_, _ = stream.Write([]byte{statusOK})
		_ = sc.Close()
		return
	}
	// loopback only, and only a port the orchestrator listed. the proxy chooses
	// the port, and a proxy that could choose 22 would be a way into the guest
	// that no fusefile declared.
	var local net.Conn
	err := fmt.Errorf("port %d is not published", h.Port)
	if slices.Contains(ports, h.Port) {
		local, err = net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(h.Port)), 5*time.Second)
	}
	if err != nil {
		logger.Warn("refusing stream", "port", h.Port, "err", err)
		_, _ = stream.Write([]byte{statusRefused})
		_ = sc.Close()
		return
	}
	if _, err := stream.Write([]byte{statusOK}); err != nil {
		_ = local.Close()
		_ = sc.Close()
		return
	}
	Pipe(local, sc)
}
