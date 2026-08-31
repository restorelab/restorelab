package checks

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/restorelab/restorelab/internal/core"
)

// startTestDNSServer runs a minimal in-process UDP DNS responder that
// answers A queries from the given name -> IPv4 map (dotted, e.g.
// "app.local" -> "10.0.0.9"). Unknown names get an empty-answer response
// (NXDOMAIN-ish but simpler: just no records). It never talks to the real
// network.
func startTestDNSServer(t *testing.T, records map[string]string) (host string, port int) {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	go func() {
		buf := make([]byte, 512)
		for {
			n, addr, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			var p dnsmessage.Parser
			hdr, err := p.Start(buf[:n])
			if err != nil {
				continue
			}
			q, err := p.Question()
			if err != nil {
				continue
			}
			name := strings.TrimSuffix(q.Name.String(), ".")

			respHdr := dnsmessage.Header{ID: hdr.ID, Response: true, Authoritative: true}
			b := dnsmessage.NewBuilder(nil, respHdr)
			_ = b.StartQuestions()
			_ = b.Question(q)
			_ = b.StartAnswers()
			if ip, ok := records[name]; ok && q.Type == dnsmessage.TypeA {
				parsed := net.ParseIP(ip).To4()
				var a [4]byte
				copy(a[:], parsed)
				_ = b.AResource(
					dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60},
					dnsmessage.AResource{A: a},
				)
			}
			msg, err := b.Finish()
			if err != nil {
				continue
			}
			_, _ = conn.WriteToUDP(msg, addr)
		}
	}()

	udpAddr := conn.LocalAddr().(*net.UDPAddr)
	return "127.0.0.1", udpAddr.Port
}

func TestDNSCheck_ARecord_Pass(t *testing.T) {
	host, port := startTestDNSServer(t, map[string]string{"app.local": "10.0.0.9"})

	cfg := core.CheckConfig{Type: "dns", Timeout: 2 * time.Second, Params: map[string]any{
		"name":   "app.local",
		"server": host,
		"port":   port,
	}}
	res := DNSCheck{}.Run(context.Background(), core.Target{}, cfg)
	if res.Status != core.CheckPass {
		t.Fatalf("Status = %v, Message = %q, want CheckPass", res.Status, res.Message)
	}
	answers, _ := res.Details["answers"].([]string)
	if len(answers) != 1 || answers[0] != "10.0.0.9" {
		t.Fatalf("answers = %v, want [10.0.0.9]", answers)
	}
}

func TestDNSCheck_NoAnswers_Fail(t *testing.T) {
	host, port := startTestDNSServer(t, map[string]string{})

	cfg := core.CheckConfig{Type: "dns", Timeout: 2 * time.Second, Params: map[string]any{
		"name":   "missing.local",
		"server": host,
		"port":   port,
	}}
	res := DNSCheck{}.Run(context.Background(), core.Target{}, cfg)
	if res.Status != core.CheckFail {
		t.Fatalf("Status = %v, Message = %q, want CheckFail", res.Status, res.Message)
	}
}

func TestDNSCheck_ExpectMatch(t *testing.T) {
	host, port := startTestDNSServer(t, map[string]string{"app.local": "10.0.0.9"})

	cfg := core.CheckConfig{Type: "dns", Timeout: 2 * time.Second, Params: map[string]any{
		"name":   "app.local",
		"server": host,
		"port":   port,
		"expect": []any{"10.0.0.9", "10.0.0.10"},
	}}
	res := DNSCheck{}.Run(context.Background(), core.Target{}, cfg)
	if res.Status != core.CheckPass {
		t.Fatalf("Status = %v, Message = %q, want CheckPass", res.Status, res.Message)
	}
}

func TestDNSCheck_ExpectMismatch(t *testing.T) {
	host, port := startTestDNSServer(t, map[string]string{"app.local": "10.0.0.9"})

	cfg := core.CheckConfig{Type: "dns", Timeout: 2 * time.Second, Params: map[string]any{
		"name":   "app.local",
		"server": host,
		"port":   port,
		"expect": []any{"10.0.0.99"},
	}}
	res := DNSCheck{}.Run(context.Background(), core.Target{}, cfg)
	if res.Status != core.CheckFail {
		t.Fatalf("Status = %v, want CheckFail", res.Status)
	}
}

func TestDNSCheck_MissingName(t *testing.T) {
	cfg := core.CheckConfig{Type: "dns", Params: map[string]any{"server": "127.0.0.1", "port": 1}}
	res := DNSCheck{}.Run(context.Background(), core.Target{}, cfg)
	if res.Status != core.CheckError {
		t.Fatalf("Status = %v, want CheckError (name is required)", res.Status)
	}
}

func TestDNSCheck_UnsupportedType(t *testing.T) {
	host, port := startTestDNSServer(t, map[string]string{})
	cfg := core.CheckConfig{Type: "dns", Timeout: 2 * time.Second, Params: map[string]any{
		"name":   "app.local",
		"server": host,
		"port":   port,
		"type":   "SRV",
	}}
	res := DNSCheck{}.Run(context.Background(), core.Target{}, cfg)
	if res.Status != core.CheckError {
		t.Fatalf("Status = %v, want CheckError (unsupported type)", res.Status)
	}
}

func TestDNSCheck_DefaultServerIsTargetIP(t *testing.T) {
	host, port := startTestDNSServer(t, map[string]string{"app.local": "10.0.0.9"})
	cfg := core.CheckConfig{Type: "dns", Timeout: 2 * time.Second, Params: map[string]any{
		"name": "app.local",
		"port": port,
	}}
	res := DNSCheck{}.Run(context.Background(), core.Target{IP: host}, cfg)
	if res.Status != core.CheckPass {
		t.Fatalf("Status = %v, Message = %q, want CheckPass", res.Status, res.Message)
	}
}
