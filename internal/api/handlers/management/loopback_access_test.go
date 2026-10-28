package management

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestPasswordlessLoopbackRequestBoundary(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	for _, tt := range []struct {
		name, bind, peer, host, origin, referer, site, forwarded string
		want                                                     bool
	}{
		{name: "local CLI", want: true},
		{name: "same origin", origin: "http://localhost:8317", site: "same-origin", want: true},
		{name: "navigation", site: "none", want: true},
		{name: "IPv6", bind: "::1", peer: "[::1]:12345", host: "[::1]:8317", origin: "http://[::1]:8317", want: true},
		{name: "IPv4 mapped peer", peer: "[::ffff:127.0.0.1]:1234", want: true},
		{name: "wildcard bind", bind: "0.0.0.0"},
		{name: "LAN bind", bind: "192.168.1.5"},
		{name: "remote peer", peer: "192.168.1.5:12345"},
		{name: "spoofed forwarded peer", peer: "192.168.1.5:12345", forwarded: "127.0.0.1"},
		{name: "reverse proxy", forwarded: "192.168.1.5"},
		{name: "DNS rebinding", host: "attacker.example:8317"},
		{name: "wrong port", host: "localhost:9000"},
		{name: "external origin", origin: "https://attacker.example"},
		{name: "opaque origin", origin: "null"},
		{name: "different local port", origin: "http://localhost:9000"},
		{name: "different scheme", origin: "https://localhost:8317"},
		{name: "external referer", referer: "https://attacker.example/page"},
		{name: "cross-site no origin", site: "cross-site"},
		{name: "same-site no origin", site: "same-site"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.bind == "" {
				tt.bind = "127.0.0.1"
			}
			if tt.peer == "" {
				tt.peer = "127.0.0.1:12345"
			}
			if tt.host == "" {
				tt.host = "localhost:8317"
			}
			cfg := &config.Config{Host: tt.bind, Port: 8317, RemoteManagement: config.RemoteManagement{AllowLocalWithoutKey: true}}
			h := NewHandler(cfg, "", nil)
			r := httptest.NewRequest(http.MethodPost, "http://"+tt.host+"/v0/management/auth-files", nil)
			r.RemoteAddr = tt.peer
			for k, v := range map[string]string{"Origin": tt.origin, "Referer": tt.referer, "Sec-Fetch-Site": tt.site, "X-Forwarded-For": tt.forwarded} {
				if v != "" {
					r.Header.Set(k, v)
				}
			}
			if got := h.LocalWithoutKeyRequestAllowed(r); got != tt.want {
				t.Fatalf("allowed=%v want=%v", got, tt.want)
			}
			engine := gin.New()
			engine.POST("/v0/management/auth-files", h.Middleware(), func(c *gin.Context) { c.Status(204) })
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, r)
			if (w.Code == 204) != tt.want {
				t.Fatalf("HTTP %d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestPasswordlessLoopbackCannotBeEnabledOnExistingWildcardListener(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	cfg := &config.Config{Host: "0.0.0.0", Port: 8317}
	h := NewHandler(cfg, "", nil)
	next := *cfg
	next.Host = "127.0.0.1"
	next.RemoteManagement.AllowLocalWithoutKey = true
	h.SetConfig(&next)
	if h.LocalWithoutKeyEnabled() {
		t.Fatal("config reload unlocked wildcard listener")
	}
	h = NewHandler(&next, "", nil)
	if !h.LocalWithoutKeyEnabled() {
		t.Fatal("loopback mode unavailable")
	}
	next.RemoteManagement.AllowRemote = true
	if h.LocalWithoutKeyEnabled() {
		t.Fatal("allow-remote must disable passwordless mode")
	}
	next.RemoteManagement.AllowRemote = false
	next.RemoteManagement.AllowLocalWithoutKey = false
	if h.LocalWithoutKeyEnabled() {
		t.Fatal("mode must be opt-in")
	}
}

func TestPasswordlessLoopbackRejectsDuplicateOriginAndDoesNotUnlockNonHTTP(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	h := NewHandler(&config.Config{Host: "127.0.0.1", Port: 8317, RemoteManagement: config.RemoteManagement{AllowLocalWithoutKey: true}}, "", nil)
	r := httptest.NewRequest("GET", "http://localhost:8317/v0/management/config", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Add("Origin", "http://localhost:8317")
	r.Header.Add("Origin", "https://attacker.example")
	if h.LocalWithoutKeyRequestAllowed(r) {
		t.Fatal("duplicate Origin accepted")
	}
	if allowed, _, _ := h.AuthenticateManagementKey("127.0.0.1", true, ""); allowed {
		t.Fatal("non-HTTP authentication bypassed")
	}
}

func TestPasswordlessLoopbackMultiplexerTLSState(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	h := NewHandler(&config.Config{Host: "127.0.0.1", Port: 8317, RemoteManagement: config.RemoteManagement{AllowLocalWithoutKey: true}}, "", nil)
	r := httptest.NewRequest("GET", "http://localhost:8317/v0/management/config", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	r.TLS = &tls.ConnectionState{}
	r.Header.Set("Origin", "http://localhost:8317")
	if !h.LocalWithoutKeyRequestAllowed(r) {
		t.Fatal("empty multiplexer TLS state must remain HTTP")
	}
	r.Header.Set("Origin", "https://localhost:8317")
	if h.LocalWithoutKeyRequestAllowed(r) {
		t.Fatal("HTTPS origin must not match a plain HTTP connection")
	}
	r.TLS.HandshakeComplete = true
	if !h.LocalWithoutKeyRequestAllowed(r) {
		t.Fatal("completed TLS handshake must match HTTPS origin")
	}
}
