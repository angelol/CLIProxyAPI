package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestQuotaDeadlineBoundedTransientRetries(t *testing.T) {
	withQuotaCooldownEnabled(t)
	for _, affinity := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, tc := range []struct {
				name                    string
				status, failures, calls int
				maxWait                 time.Duration
				success, cancel         bool
			}{
				{name: "recovers after all eligible accounts fail", status: 503, failures: 2, calls: 3, maxWait: 30 * time.Second, success: true},
				{name: "overload recovers", status: 529, failures: 2, calls: 3, maxWait: 30 * time.Second, success: true},
				{name: "persistent failures stop after two extra rounds", status: 503, failures: 100, calls: 6, maxWait: 30 * time.Second},
				{name: "wait longer than budget stops", status: 503, failures: 100, calls: 2, maxWait: 5 * time.Second},
				{name: "client cancellation stops waiting", status: 503, failures: 100, calls: 2, maxWait: 30 * time.Second, cancel: true},
			} {
				t.Run(fmt.Sprintf("%s/affinity=%t/stream=%t", tc.name, affinity, stream), func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						var selector Selector = &QuotaDeadlineSelector{}
						if affinity {
							s := NewSessionAffinitySelector(selector)
							defer s.Stop()
							selector = s
						}
						manager := NewManager(nil, selector, nil)
						manager.SetRetryConfig(2, tc.maxWait, 0)
						model := "claude-fable-quota-retry-test"
						now := time.Now()
						for i, used := range []float64{.1, .2, 1} {
							a := deadlineAuth(fmt.Sprintf("%s/%d", t.Name(), i), now, used, 24*time.Hour)
							registry.GetGlobalRegistry().RegisterClient(a.ID, "claude", []*registry.ModelInfo{{ID: model}})
							defer registry.GetGlobalRegistry().UnregisterClient(a.ID)
							if _, err := manager.Register(context.Background(), a); err != nil {
								t.Fatal(err)
							}
						}
						ctx, cancel := context.WithCancel(context.Background())
						if tc.cancel {
							timer := time.AfterFunc(time.Second, cancel)
							defer timer.Stop()
						}
						defer cancel()
						calls := 0
						retryAfter := 10 * time.Second
						attempt := func(_ context.Context, a *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
							if a.Quota.Signals[http.CanonicalHeaderKey("Anthropic-Ratelimit-Unified-7d-Utilization")] == "1" {
								t.Error("attempted exhausted account")
							}
							calls++
							if calls <= tc.failures {
								return cliproxyexecutor.Response{}, customStatusError{code: tc.status, msg: "temporary upstream failure", retryAfter: &retryAfter}
							}
							return cliproxyexecutor.Response{Payload: []byte("ok")}, nil
						}
						manager.RegisterExecutor(&customStreamMockExecutor{
							identifier: "claude", mockCustomErrorExecutor: mockCustomErrorExecutor{executeFn: attempt},
							streamFn: func(ctx context.Context, a *Auth, r cliproxyexecutor.Request, o cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
								response, err := attempt(ctx, a, r, o)
								if err != nil {
									return nil, err
								}
								chunks := make(chan cliproxyexecutor.StreamChunk, 1)
								chunks <- cliproxyexecutor.StreamChunk{Payload: response.Payload}
								close(chunks)
								return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
							},
						})
						opts := deadlineOptions("retry-session")
						opts.Stream = stream
						var err error
						if stream {
							var result *cliproxyexecutor.StreamResult
							result, err = manager.ExecuteStream(ctx, []string{"claude"}, cliproxyexecutor.Request{Model: model}, opts)
							if err == nil {
								for chunk := range result.Chunks {
									if chunk.Err != nil {
										err = chunk.Err
									}
								}
							}
						} else {
							_, err = manager.Execute(ctx, []string{"claude"}, cliproxyexecutor.Request{Model: model}, opts)
						}
						if (err == nil) != tc.success {
							t.Errorf("error=%v success=%t", err, tc.success)
						}
						if tc.cancel && !errors.Is(err, context.Canceled) {
							t.Errorf("expected cancellation, got %v", err)
						}
						if calls != tc.calls {
							t.Errorf("attempts=%d, want %d", calls, tc.calls)
						}
						elapsed := time.Since(now)
						if tc.success && elapsed < retryAfter {
							t.Errorf("retry did not wait for recovery: %v", elapsed)
						}
						if elapsed > 2*tc.maxWait {
							t.Errorf("exceeded bounded retry waits: %v", elapsed)
						}
					})
				})
			}
		}
	}
}
