package cliproxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"fmt"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

const testClaudeUsage = `{"five_hour":{"utilization":12,"resets_at":"2026-10-28T16:29:59.848538Z"},"seven_day":{"utilization":67,"resets_at":"2026-10-29T18:59:59.848562Z"},"seven_day_opus":{"utilization":100},"extra_usage":{"is_enabled":false}}`

type usageTransport func(*http.Request) (*http.Response, error)

func (f usageTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestClaudeUsageFetchDoesNotSendInference(t *testing.T) {
	called := 0
	a := &coreauth.Auth{Metadata: map[string]any{"access_token": "test-token"}}
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", usageTransport(func(r *http.Request) (*http.Response, error) {
		called++
		if r.Method != http.MethodGet || r.URL.String() != claudeUsageURL || r.Body != nil || r.Header.Get("Authorization") != "Bearer test-token" || r.Header.Get("anthropic-beta") != "oauth-2025-04-20" {
			t.Fatal("unexpected usage request")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(testClaudeUsage))}, nil
	}))
	h, err := fetchClaudeUsage(ctx, &config.Config{}, a)
	if err != nil || called != 1 || h.Get("Anthropic-Ratelimit-Unified-7d-Utilization") != "0.67" || h.Get("Anthropic-Ratelimit-Unified-5h-Utilization") != "0.12" || h.Get("Anthropic-Ratelimit-Unified-7d-Status") != "allowed" {
		t.Fatalf("headers=%v err=%v calls=%d", h, err, called)
	}
}

func TestClaudeUsageFetchRejectsRedirectAndHTTPFailures(t *testing.T) {
	for _, status := range []int{302, 401, 403, 429, 500} {
		calls := 0
		ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", usageTransport(func(r *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: status, Header: http.Header{"Location": {"https://other.example/"}}, Body: io.NopCloser(strings.NewReader("sensitive error body"))}, nil
		}))
		_, err := fetchClaudeUsage(ctx, &config.Config{}, &coreauth.Auth{Metadata: map[string]any{"access_token": "test-token"}})
		if err == nil || calls != 1 || strings.Contains(err.Error(), "sensitive") {
			t.Fatalf("status=%d calls=%d err=%v", status, calls, err)
		}
	}
}

func TestClaudeUsageValidation(t *testing.T) {
	for _, body := range []string{`{}`, `{"five_hour":null,"seven_day":null}`, `{"seven_day":{"utilization":-1,"resets_at":"2026-10-29T18:59:59Z"}}`, `{"seven_day":{"utilization":67,"resets_at":"invalid"}}`, `{"seven_day":{"resets_at":"2026-10-29T18:59:59Z"}}`, `{"seven_day":{"utilization":NaN}}`, `{"extra_usage":{"utilization":10}}`} {
		if _, err := claudeUsageHeaders([]byte(body)); err == nil {
			t.Fatalf("accepted malformed or non-shared quota: %s", body)
		}
	}
	h, err := claudeUsageHeaders([]byte(`{"five_hour":{"utilization":0,"resets_at":null},"seven_day":{"utilization":100,"resets_at":"2026-10-29T18:59:59Z"}}`))
	if err != nil || h.Get("Anthropic-Ratelimit-Unified-7d-Status") != "rejected" || h.Get("Anthropic-Ratelimit-Unified-5h-Reset") != "" {
		t.Fatalf("inactive session/exhausted week: %v %v", h, err)
	}
}

func TestQuotaRefreshScopeAndFailurePreserveObservation(t *testing.T) {
	m := coreauth.NewManager(nil, &coreauth.QuotaDeadlineSelector{}, nil)
	s := &Service{cfg: &config.Config{}, coreManager: m}
	s.cfg.Routing.Strategy = "quota-deadline"
	for _, a := range []*coreauth.Auth{
		{ID: "oauth", Provider: "claude", Metadata: map[string]any{"access_token": "test"}},
		{ID: "disabled", Provider: "claude", Disabled: true, Metadata: map[string]any{"access_token": "test"}},
		{ID: "codex", Provider: "codex", Metadata: map[string]any{"access_token": "test"}},
		{ID: "no-token", Provider: "claude"},
		{ID: "custom-base", Provider: "claude", Attributes: map[string]string{"base_url": "https://other.example"}, Metadata: map[string]any{"access_token": "test"}},
		{ID: "key", Provider: "claude", Attributes: map[string]string{"api_key": "test"}},
	} {
		if _, err := m.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	fetch := func(ctx context.Context, cfg *config.Config, a *coreauth.Auth) (http.Header, error) {
		calls++
		if a.ID != "oauth" {
			t.Fatalf("polled %s", a.ID)
		}
		return claudeUsageHeaders([]byte(testClaudeUsage))
	}
	s.refreshClaudeUsage(context.Background(), fetch)
	a, _ := m.GetByID("oauth")
	before := a.Quota.ObservedAt
	if calls != 1 || before.IsZero() {
		t.Fatal("OAuth account not refreshed")
	}
	s.refreshClaudeUsage(context.Background(), func(context.Context, *config.Config, *coreauth.Auth) (http.Header, error) {
		return nil, errors.New("unavailable")
	})
	a, _ = m.GetByID("oauth")
	if !a.Quota.ObservedAt.Equal(before) {
		t.Fatal("failure replaced observation")
	}
	s.cfg.Routing.Strategy = "round-robin"
	s.refreshClaudeUsage(context.Background(), fetch)
	s.cfg.Routing.Strategy = "quota-deadline"
	s.cfg.Home.Enabled = true
	s.refreshClaudeUsage(context.Background(), fetch)
	s.cfg.Home.Enabled = false
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.refreshClaudeUsage(ctx, fetch)
	if calls != 1 {
		t.Fatal("polled disabled strategy, Home, or canceled context")
	}
}

func TestQuotaRefreshStopsInFlightRequest(t *testing.T) {
	m := coreauth.NewManager(nil, &coreauth.QuotaDeadlineSelector{}, nil)
	_, err := m.Register(context.Background(), &coreauth.Auth{ID: "oauth", Provider: "claude", Metadata: map[string]any{"access_token": "test"}})
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{cfg: &config.Config{}, coreManager: m}
	s.cfg.Routing.Strategy = "quota-deadline"
	started := make(chan struct{})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", usageTransport(func(r *http.Request) (*http.Response, error) {
		close(started)
		<-r.Context().Done()
		return nil, r.Context().Err()
	}))
	s.startQuotaRefresh(ctx)
	defer s.stopQuotaRefresh()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("poll did not start")
	}
	done := make(chan struct{})
	go func() { s.stopQuotaRefresh(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not cancel poll")
	}
}

func TestQuotaRefreshChangesNewPlacementsNotWarmBindings(t *testing.T) {
	selector := coreauth.NewSessionAffinitySelector(&coreauth.QuotaDeadlineSelector{})
	defer selector.Stop()
	m := coreauth.NewManager(nil, selector, nil)
	m.RegisterExecutor(runtimeexecutor.NewClaudeExecutor(&config.Config{}))
	s := &Service{cfg: &config.Config{}, coreManager: m}
	s.cfg.Routing.Strategy = "quota-deadline"
	const model = "claude-quota-poll-test"
	for _, id := range []string{"poll-a", "poll-b"} {
		_, err := m.Register(context.Background(), &coreauth.Auth{ID: id, Provider: "claude", Metadata: map[string]any{"access_token": "test"}})
		if err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: model}})
		m.RefreshSchedulerEntry(id)
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	favored := "poll-a"
	fetch := func(ctx context.Context, cfg *config.Config, a *coreauth.Auth) (http.Header, error) {
		hours := 96
		if a.ID == favored {
			hours = 12
		}
		return claudeUsageHeaders([]byte(fmt.Sprintf(`{"seven_day":{"utilization":10,"resets_at":%q}}`, time.Now().Add(time.Duration(hours)*time.Hour).Format(time.RFC3339Nano))))
	}
	pick := func(session, want string) {
		t.Helper()
		got, err := m.SelectAuth(context.Background(), "claude", model, cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": {session}}})
		if err != nil || got == nil || got.ID != want {
			t.Fatalf("session=%s got=%v err=%v want=%s", session, got, err, want)
		}
	}
	s.refreshClaudeUsage(context.Background(), fetch)
	pick("warm-session", "poll-a")
	favored = "poll-b"
	s.refreshClaudeUsage(context.Background(), fetch)
	pick("warm-session", "poll-a")
	pick("new-session", "poll-b")
}
