package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestClaudeBindingBetaFollowsFinalBody(t *testing.T) {
	for _, mode := range []string{"native", "caller", "cloaked"} {
		for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
			for _, binding := range []bool{false, true} {
				for _, alreadyRequested := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/binding=%t/requested=%t", mode, path, binding, alreadyRequested), func(t *testing.T) {
						auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key"}}
						if mode == "cloaked" {
							auth.Attributes["fingerprint_profile"] = "claude-code-cli"
						}
						incoming := http.Header{"Anthropic-Beta": {"claude-code-20250219,caller-feature-2030-01-01"}}
						if alreadyRequested {
							incoming.Set("Anthropic-Beta", incoming.Get("Anthropic-Beta")+","+claudeThinkingBindingBeta)
						}
						body := []byte(`{"model":"claude-fable-5-2","messages":[{"role":"user","content":"hi"}]}`)
						if binding {
							body = []byte(`{"model":"claude-fable-5-2","thinking":{"type":"adaptive","block_binding":{"prefix_mismatch_behavior":"drop_block"}},"messages":[{"role":"user","content":"hi"}]}`)
						}
						req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com"+path, nil)
						if err := applyClaudeHeaders(req, auth, "test-key", false, nil, body, &config.Config{}, incoming, mode == "native"); err != nil {
							t.Fatal(err)
						}
						betas := req.Header.Get("Anthropic-Beta")
						count := 0
						for _, beta := range strings.Split(betas, ",") {
							if beta == claudeThinkingBindingBeta {
								count++
							}
						}
						if binding && count != 1 {
							t.Fatalf("body requires exactly one binding beta, count=%d header=%q", count, betas)
						}
						if !binding && !alreadyRequested && count != 0 {
							t.Fatalf("binding beta added to a request without binding: %q", betas)
						}
						if mode == "native" || mode == "caller" {
							if !strings.Contains(betas, "caller-feature-2030-01-01") {
								t.Fatal("caller beta lost")
							}
						}
					})
				}
			}
		}
	}
}

func TestClaudeExecutorNativeThinkingReplayCarriesBindingBeta(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			calls := 0
			transport := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				if got := gjson.GetBytes(body, "thinking.block_binding.prefix_mismatch_behavior").String(); got != "drop_block" {
					t.Fatalf("missing replay binding: %q", got)
				}
				status := http.StatusOK
				contentType := "application/json"
				response := `{"id":"msg_binding","type":"message","model":"claude-fable-5-2","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`
				if stream {
					contentType = "text/event-stream"
					response = "data: {\"type\":\"message_stop\"}\n\n"
				}
				if !strings.Contains(helps.HeaderValueCaseInsensitive(r.Header, "Anthropic-Beta"), claudeThinkingBindingBeta) {
					status = http.StatusBadRequest
					contentType = "application/json"
					response = `{"type":"error","error":{"type":"invalid_request_error","message":"thinking.adaptive.block_binding: Extra inputs are not permitted"}}`
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(response)), Request: r}, nil
			})
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
			executor := NewClaudeExecutor(&config.Config{})
			auth := claudeNativeHelperOAuthAuth("https://api.anthropic.com")
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Headers: claudeNativeHelperHeaders("claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14", "identity")}
			req := cliproxyexecutor.Request{Model: "claude-fable-5-2", Payload: preservedThinkingReplayPayload("claude-fable-5-2")}
			req.Payload, _ = sjson.SetBytes(req.Payload, "metadata.user_id", claudeNativeHelperUserID)
			if !helps.DetectClaudeCodeRequest(opts.Headers, req.Payload, false).Confirmed {
				t.Fatal("fixture must take the native Claude Code path")
			}
			if stream {
				result, err := executor.ExecuteStream(ctx, auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
				}
			} else {
				if _, err := executor.Execute(ctx, auth, req, opts); err != nil {
					t.Fatal(err)
				}
			}
			if calls != 1 {
				t.Fatalf("upstream calls=%d, want one", calls)
			}
		})
	}
}
