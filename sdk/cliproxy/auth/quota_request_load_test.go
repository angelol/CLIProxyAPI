package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestQuotaRequestLoadInfluencesNewSessionsOnly(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	q := &QuotaDeadlineSelector{nowFunc: func() time.Time { return now }}
	s := NewSessionAffinitySelector(q)
	defer s.Stop()
	m := NewManager(nil, s, nil)
	m.RegisterExecutor(&customStreamMockExecutor{identifier: "claude"})
	const model = "claude-weighted-load"
	for _, id := range []string{"weighted-a", "weighted-b"} {
		_, err := m.Register(context.Background(), deadlineAuth(id, now, .2, 48*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	pick := func(session string) string {
		t.Helper()
		a, err := m.SelectAuth(context.Background(), "claude", model, cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": {session}}})
		if err != nil {
			t.Fatal(err)
		}
		return a.ID
	}
	if pick("warm") != "weighted-a" {
		t.Fatal("unexpected initial binding")
	}
	for range 3 {
		defer m.quotaLoad.begin("weighted-a")()
	}
	for range 40 {
		if pick("warm") != "weighted-a" {
			t.Fatal("load broke warm affinity")
		}
	}
	counts := map[string]int{}
	for i := range 100 {
		counts[pick(fmt.Sprintf("fresh-%d", i))]++
	}
	if counts["weighted-a"] < 19 || counts["weighted-a"] > 21 {
		t.Fatalf("load did not influence manager selection: %v", counts)
	}
}

func TestQuotaRequestLoadExecutionLifecycle(t *testing.T) {
	for _, mode := range []string{"success", "error", "stream", "stream-error", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			m := NewManager(nil, &QuotaDeadlineSelector{}, nil)
			m.SetRetryConfig(0, 0, 0)
			a := deadlineAuth(t.Name(), time.Now(), .2, 48*time.Hour)
			_, err := m.Register(context.Background(), a)
			if err != nil {
				t.Fatal(err)
			}
			const model = "claude-load-test"
			registry.GetGlobalRegistry().RegisterClient(a.ID, "claude", []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
			chunks := make(chan cliproxyexecutor.StreamChunk, 1)
			chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("first")}
			if mode != "cancel" {
				close(chunks)
			}
			m.RegisterExecutor(&customStreamMockExecutor{identifier: "claude", mockCustomErrorExecutor: mockCustomErrorExecutor{executeFn: func(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
				if m.quotaLoad.count(a.ID) != 1 {
					t.Error("request not counted while executing")
				}
				if mode == "error" {
					return cliproxyexecutor.Response{}, errors.New("failed")
				}
				return cliproxyexecutor.Response{}, nil
			}}, streamFn: func(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
				if m.quotaLoad.count(a.ID) != 1 {
					t.Error("stream setup not counted")
				}
				if mode == "stream-error" {
					return nil, errors.New("setup failed")
				}
				return &cliproxyexecutor.StreamResult{Headers: http.Header{"Test": {"value"}}, Chunks: chunks}, nil
			}})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "success" || mode == "error" {
				_, err = m.Execute(ctx, []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
				if (err != nil) != (mode == "error") {
					t.Fatalf("unexpected error %v", err)
				}
			} else {
				result, err := m.ExecuteStream(ctx, []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
				if mode == "stream-error" {
					if err == nil {
						t.Fatal("missing setup error")
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					if mode == "cancel" {
						if m.quotaLoad.count(a.ID) != 1 {
							t.Fatal("live stream released early")
						}
						cancel()
						close(chunks)
					}
					for range result.Chunks {
					}
				}
			}
			if got := m.quotaLoad.count(a.ID); got != 0 {
				t.Fatalf("leaked %d active requests", got)
			}
		})
	}
}

func TestQuotaRequestLoadFailoverReleasesPreviousAccount(t *testing.T) {
	m := NewManager(nil, &QuotaDeadlineSelector{}, nil)
	m.SetRetryConfig(0, 0, 0)
	const model = "claude-load-failover"
	a, b := deadlineAuth("load-a", time.Now(), .2, 24*time.Hour), deadlineAuth("load-b", time.Now(), .2, 48*time.Hour)
	for _, auth := range []*Auth{a, b} {
		_, err := m.Register(context.Background(), auth)
		if err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	}
	m.RegisterExecutor(&customStreamMockExecutor{identifier: "claude", mockCustomErrorExecutor: mockCustomErrorExecutor{executeFn: func(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
		if auth.ID == a.ID {
			return cliproxyexecutor.Response{}, errors.New("retry another account")
		}
		if m.quotaLoad.count(a.ID) != 0 || m.quotaLoad.count(b.ID) != 1 {
			t.Error("retry retained previous account's request load")
		}
		return cliproxyexecutor.Response{}, nil
	}}})
	_, err := m.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if m.quotaLoad.count(a.ID) != 0 || m.quotaLoad.count(b.ID) != 0 {
		t.Fatal("load leaked after failover")
	}
}
