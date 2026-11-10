package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/redisqueue"
)

func loopbackTestRequest(s *Server, path, origin string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "http://localhost:0"+path, nil)
	r.RemoteAddr = "127.0.0.1:4567"
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, r)
	return w
}

func TestLocalManagementHotReloadAndUsageQueue(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	previous := redisqueue.Enabled()
	t.Cleanup(func() { redisqueue.SetEnabled(previous) })
	s := newTestServerWithConfig(t, &config.Config{Host: "127.0.0.1"})
	if w := loopbackTestRequest(s, "/v0/management/routing/strategy", ""); w.Code != 404 {
		t.Fatalf("default: %d", w.Code)
	}
	next := *s.cfg
	next.RemoteManagement.AllowLocalWithoutKey = true
	next.Routing.Strategy = "quota-deadline"
	s.UpdateClients(&next)
	if w := loopbackTestRequest(s, "/v0/management/routing/strategy", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "quota-deadline") {
		t.Fatalf("enabled: %d %s", w.Code, w.Body.String())
	}
	if !redisqueue.Enabled() {
		t.Fatal("usage queue not enabled")
	}
	redisqueue.Enqueue([]byte(`{"test":true}`))
	if w := loopbackTestRequest(s, "/v0/management/usage-queue?count=1", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"test":true`) {
		t.Fatalf("usage: %d %s", w.Code, w.Body.String())
	}
	if w := loopbackTestRequest(s, "/v0/management/config", "https://attacker.example"); w.Code != 403 {
		t.Fatalf("cross-origin: %d", w.Code)
	}
	disabled := next
	disabled.RemoteManagement.AllowLocalWithoutKey = false
	s.UpdateClients(&disabled)
	if w := loopbackTestRequest(s, "/v0/management/routing/strategy", ""); w.Code != 404 {
		t.Fatalf("disabled: %d", w.Code)
	}
	if redisqueue.Enabled() {
		t.Fatal("usage queue stayed enabled")
	}
	// Changing the configured host alone cannot unlock an existing wildcard bind.
	wild := newTestServerWithConfig(t, &config.Config{Host: "0.0.0.0"})
	claimed := *wild.cfg
	claimed.Host = "127.0.0.1"
	claimed.RemoteManagement.AllowLocalWithoutKey = true
	wild.UpdateClients(&claimed)
	if w := loopbackTestRequest(wild, "/v0/management/config", ""); w.Code != 404 {
		t.Fatalf("wildcard listener: %d", w.Code)
	}
}

func TestLocalManagementPanelBootstrap(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	dir := t.TempDir()
	t.Setenv("MANAGEMENT_STATIC_PATH", dir)
	if err := os.WriteFile(filepath.Join(dir, "management.html"), []byte("<html><head></head><body>upstream panel</body></html>"), 0600); err != nil {
		t.Fatal(err)
	}
	previous := redisqueue.Enabled()
	t.Cleanup(func() { redisqueue.SetEnabled(previous) })
	s := newTestServerWithConfig(t, &config.Config{Host: "127.0.0.1", RemoteManagement: config.RemoteManagement{AllowLocalWithoutKey: true, DisableAutoUpdatePanel: true}})
	w := loopbackTestRequest(s, "/management.html", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "loopback-local-management") {
		t.Fatalf("bootstrap: %d %s", w.Code, w.Body.String())
	}
	if w.Header().Get("X-Frame-Options") != "DENY" || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("unsafe headers: %v", w.Header())
	}
	if w := loopbackTestRequest(s, "/management.html", "https://attacker.example"); w.Code != 403 {
		t.Fatalf("cross-origin UI: %d", w.Code)
	}
	next := *s.cfg
	next.RemoteManagement.AllowLocalWithoutKey = false
	s.UpdateClients(&next)
	w = loopbackTestRequest(s, "/management.html", "")
	if strings.Contains(w.Body.String(), "loopback-local-management") {
		t.Fatal("bootstrap injected outside local mode")
	}
}
