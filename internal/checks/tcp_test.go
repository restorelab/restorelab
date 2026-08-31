package checks

import (
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

func listenTCP(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

func hostPort(t *testing.T, addr net.Addr) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr.String())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	return host, port
}

func TestTCPCheck_Pass(t *testing.T) {
	ln := listenTCP(t)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	host, port := hostPort(t, ln.Addr())
	cfg := core.CheckConfig{Type: "tcp", Timeout: 2 * time.Second, Params: map[string]any{
		"host": host,
		"port": port,
	}}
	res := TCPCheck{}.Run(context.Background(), core.Target{}, cfg)
	if res.Status != core.CheckPass {
		t.Fatalf("Status = %v, Message = %q, want CheckPass", res.Status, res.Message)
	}
	if _, ok := res.Details["latency_ms"]; !ok {
		t.Fatal("expected latency_ms in details")
	}
}

func TestTCPCheck_Fail_ClosedPort(t *testing.T) {
	ln := listenTCP(t)
	host, port := hostPort(t, ln.Addr())
	ln.Close() // close it immediately so the port is refused

	cfg := core.CheckConfig{Type: "tcp", Timeout: 2 * time.Second, Params: map[string]any{
		"host": host,
		"port": port,
	}}
	res := TCPCheck{}.Run(context.Background(), core.Target{}, cfg)
	if res.Status != core.CheckFail {
		t.Fatalf("Status = %v, Message = %q, want CheckFail", res.Status, res.Message)
	}
	if !strings.Contains(res.Message, strconv.Itoa(port)) {
		t.Fatalf("Message should mention the port, got: %q", res.Message)
	}
}

func TestTCPCheck_BannerMatch(t *testing.T) {
	ln := listenTCP(t)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("SSH-2.0-OpenSSH_9.6\r\n"))
	}()

	host, port := hostPort(t, ln.Addr())
	cfg := core.CheckConfig{Type: "tcp", Timeout: 2 * time.Second, Params: map[string]any{
		"host":          host,
		"port":          port,
		"expect_banner": "SSH-2.0",
	}}
	res := TCPCheck{}.Run(context.Background(), core.Target{}, cfg)
	if res.Status != core.CheckPass {
		t.Fatalf("Status = %v, Message = %q, want CheckPass", res.Status, res.Message)
	}
	if res.Details["banner"] == nil {
		t.Fatal("expected banner in details")
	}
}

func TestTCPCheck_BannerMismatch(t *testing.T) {
	ln := listenTCP(t)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\n"))
	}()

	host, port := hostPort(t, ln.Addr())
	cfg := core.CheckConfig{Type: "tcp", Timeout: 2 * time.Second, Params: map[string]any{
		"host":          host,
		"port":          port,
		"expect_banner": "SSH-2.0",
	}}
	res := TCPCheck{}.Run(context.Background(), core.Target{}, cfg)
	if res.Status != core.CheckFail {
		t.Fatalf("Status = %v, want CheckFail", res.Status)
	}
}

func TestTCPCheck_MissingPort(t *testing.T) {
	cfg := core.CheckConfig{Type: "tcp", Params: map[string]any{"host": "127.0.0.1"}}
	res := TCPCheck{}.Run(context.Background(), core.Target{}, cfg)
	if res.Status != core.CheckError {
		t.Fatalf("Status = %v, want CheckError (port is required)", res.Status)
	}
}

func TestTCPCheck_DefaultsHostToTargetIP(t *testing.T) {
	ln := listenTCP(t)
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			conn.Close()
		}
	}()
	host, port := hostPort(t, ln.Addr())

	cfg := core.CheckConfig{Type: "tcp", Timeout: 2 * time.Second, Params: map[string]any{"port": port}}
	res := TCPCheck{}.Run(context.Background(), core.Target{IP: host}, cfg)
	if res.Status != core.CheckPass {
		t.Fatalf("Status = %v, Message = %q, want CheckPass", res.Status, res.Message)
	}
}
