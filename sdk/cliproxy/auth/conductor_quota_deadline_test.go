package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestManagerQuotaDeadlineExecutionAndFeedback(t *testing.T) {
	withQuotaCooldownEnabled(t)
	for _, affinity := range []bool{false, true} {
		for _, path := range []string{"select", "execute", "stream"} {
			name := path
			if affinity {
				name += "-affinity"
			}
			t.Run(name, func(t *testing.T) {
				now := time.Now()
				var s Selector = &QuotaDeadlineSelector{}
				if affinity {
					a := NewSessionAffinitySelector(s)
					defer a.Stop()
					s = a
				}
				manager := NewManager(nil, s, nil)
				manager.SetRetryConfig(0, 0, 0)
				const route, upstream = "deadline-alias", "claude-sonnet-deadline-test"
				manager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{"claude": {{Name: upstream, Alias: route, Fork: true}}})
				a := deadlineAuth(t.Name()+"-early", now, .2, 24*time.Hour)
				b := deadlineAuth(t.Name()+"-late", now, .3, 96*time.Hour)
				for _, candidate := range []*Auth{a, b} {
					// A stale cooldown under the alias must not block its healthy target.
					candidate.ModelStates = map[string]*ModelState{route: {Unavailable: true, NextRetryAfter: now.Add(time.Hour)}}
					if _, err := manager.Register(context.Background(), candidate); err != nil {
						t.Fatal(err)
					}
					registry.GetGlobalRegistry().RegisterClient(candidate.ID, "claude", []*registry.ModelInfo{{ID: route}, {ID: upstream}})
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(candidate.ID) })
				}
				execute := func(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
					if req.Model != upstream {
						t.Errorf("upstream model=%q", req.Model)
					}
					headers := http.Header{}
					for k, v := range deadlineAuth("", now, .999, 24*time.Hour).Quota.Signals {
						headers.Set(k, v)
					}
					logging.SetResponseHeaders(ctx, headers)
					return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
				}
				manager.RegisterExecutor(&customStreamMockExecutor{
					identifier: "claude", mockCustomErrorExecutor: mockCustomErrorExecutor{executeFn: execute},
					streamFn: func(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
						response, err := execute(ctx, auth, req, opts)
						if err != nil {
							return nil, err
						}
						chunks := make(chan cliproxyexecutor.StreamChunk, 1)
						chunks <- cliproxyexecutor.StreamChunk{Payload: response.Payload}
						close(chunks)
						return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
					},
				})
				run := func() string {
					t.Helper()
					ctx := context.Background()
					opts := deadlineOptions("explicit")
					switch path {
					case "select":
						got, err := manager.SelectAuth(ctx, "claude", route, opts)
						if err != nil {
							t.Fatal(err)
						}
						return got.ID
					case "execute":
						got, err := manager.Execute(ctx, []string{"claude"}, cliproxyexecutor.Request{Model: route}, opts)
						if err != nil {
							t.Fatal(err)
						}
						return string(got.Payload)
					default:
						opts.Stream = true
						got, err := manager.ExecuteStream(ctx, []string{"claude"}, cliproxyexecutor.Request{Model: route}, opts)
						if err != nil {
							t.Fatal(err)
						}
						var out []byte
						for chunk := range got.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
							out = append(out, chunk.Payload...)
						}
						return string(out)
					}
				}
				if got := run(); got != a.ID {
					t.Fatalf("first selection=%q want %q", got, a.ID)
				}
				if path == "select" {
					ctx := logging.WithResponseHeadersHolder(context.Background())
					headers := http.Header{}
					for k, v := range deadlineAuth("", now, .999, 24*time.Hour).Quota.Signals {
						headers.Set(k, v)
					}
					logging.SetResponseHeaders(ctx, headers)
					manager.MarkResult(ctx, Result{AuthID: a.ID, Provider: "claude", Model: upstream, Success: true})
				}
				stored, _ := manager.GetByID(a.ID)
				if got := quotaSignal(stored.Quota.Signals, "Anthropic-Ratelimit-Unified-7d-Utilization"); got != "0.999" {
					t.Fatalf("response observation=%q", got)
				}
				if got := run(); got != b.ID {
					t.Fatalf("feedback did not rebalance: got %q want %q", got, b.ID)
				}
				retry := time.Hour
				manager.MarkResult(context.Background(), Result{AuthID: b.ID, Provider: "claude", Model: upstream, RetryAfter: &retry, Error: &Error{HTTPStatus: 429, Message: "quota"}})
				if got := run(); got != a.ID {
					t.Fatalf("cooldown did not fail over: %q", got)
				}
			})
		}
	}
}
