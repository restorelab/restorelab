package pbs

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestListDatastores(t *testing.T) {
	srv, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api2/json/admin/datastore" {
			t.Fatalf("path = %q, want /api2/json/admin/datastore", r.URL.Path)
		}
		w.Write([]byte(`{"data":[
			{"store":"main","path":"/mnt/pbs/main","comment":"primary"},
			{"store":"offsite","path":"/mnt/pbs/offsite"}
		]}`))
	})

	p := newTestProvider(t, srv.URL, nil)
	stores, err := p.ListDatastores(t.Context())
	if err != nil {
		t.Fatalf("ListDatastores: %v", err)
	}
	if len(stores) != 2 {
		t.Fatalf("got %d datastores, want 2", len(stores))
	}
	if stores[0].Name != "main" || stores[0].Path != "/mnt/pbs/main" || stores[0].Comment != "primary" {
		t.Fatalf("stores[0] = %+v", stores[0])
	}
	if stores[1].Name != "offsite" {
		t.Fatalf("stores[1] = %+v", stores[1])
	}
}

func TestNormalizeFingerprint(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{name: "colon separated", in: strings.Repeat("AB:", 31) + "CD", wantErr: false},
		{name: "no separators", in: strings.ToUpper(strings.Repeat("ab", 32)), wantErr: false},
		{name: "too short", in: "AA:BB", wantErr: true},
		{name: "not hex", in: strings.Repeat("zz:", 32), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := normalizeFingerprint(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("normalizeFingerprint(%q) err = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
		})
	}
}

// TestFingerprintPinning proves the Fingerprint config option actually
// authenticates a self-signed server: a matching fingerprint succeeds, a
// mismatched one fails, even though the server's certificate would never
// pass normal chain validation.
func TestFingerprintPinning(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"version":"3.2"}}`))
	}))
	defer srv.Close()

	leaf := srv.Certificate()
	sum := sha256.Sum256(leaf.Raw)
	fingerprint := hex.EncodeToString(sum[:])

	t.Run("matching fingerprint succeeds despite self-signed cert", func(t *testing.T) {
		p := newTestProvider(t, srv.URL, func(c *Config) { c.Fingerprint = fingerprint })
		if err := p.Ping(t.Context()); err != nil {
			t.Fatalf("Ping with correct fingerprint: %v", err)
		}
	})

	t.Run("mismatched fingerprint fails", func(t *testing.T) {
		wrong := strings.Repeat("00", sha256.Size)
		p := newTestProvider(t, srv.URL, func(c *Config) { c.Fingerprint = wrong })
		if err := p.Ping(t.Context()); err == nil {
			t.Fatalf("expected Ping to fail with wrong fingerprint")
		}
	})

	t.Run("no fingerprint and no InsecureSkipVerify fails normal verification", func(t *testing.T) {
		p := newTestProvider(t, srv.URL, nil)
		if err := p.Ping(t.Context()); err == nil {
			t.Fatalf("expected Ping to fail: self-signed cert with no pinning and no InsecureSkipVerify")
		}
	})
}

func TestBuildTLSConfig_InsecureSkipVerifyWithoutFingerprint(t *testing.T) {
	cfg, err := buildTLSConfig(Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}
	if !cfg.InsecureSkipVerify {
		t.Fatalf("InsecureSkipVerify = false, want true")
	}
	if cfg.VerifyPeerCertificate != nil {
		t.Fatalf("VerifyPeerCertificate should be nil when only InsecureSkipVerify is set")
	}
}

func TestBuildTLSConfig_InvalidCACert(t *testing.T) {
	_, err := buildTLSConfig(Config{CACertPEM: "not a pem certificate"})
	if err == nil {
		t.Fatalf("expected error for invalid CACertPEM")
	}
}

// tlsVersionSanity is a light guard against accidentally regressing to an
// ancient TLS floor; not part of the spec but cheap insurance.
func TestBuildTLSConfig_DoesNotForceLegacyTLS(t *testing.T) {
	cfg, err := buildTLSConfig(Config{})
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}
	if cfg.MinVersion != 0 && cfg.MinVersion < tls.VersionTLS12 {
		t.Fatalf("MinVersion = %v, want unset or >= TLS1.2", cfg.MinVersion)
	}
}
