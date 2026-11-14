package helps

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestClaudeFableRejectionWithoutFiveHourWindow(t *testing.T) {
	now := time.Date(2026, 11, 13, 7, 42, 12, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		modify  func(http.Header)
		unified bool
	}{
		{name: "captured weekly warning and absent five-hour window"},
		{name: "weekly allowed", modify: func(h http.Header) { h.Set("Anthropic-Ratelimit-Unified-7d-Status", "allowed") }},
		{name: "five-hour rejected", unified: true, modify: func(h http.Header) { h.Set("Anthropic-Ratelimit-Unified-5h-Status", "rejected") }},
		{name: "weekly rejected", unified: true, modify: func(h http.Header) { h.Set("Anthropic-Ratelimit-Unified-7d-Status", "rejected") }},
		{name: "five-hour exhausted without status", unified: true, modify: func(h http.Header) { h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "1") }},
		{name: "malformed five-hour utilization", unified: true, modify: func(h http.Header) { h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "NaN") }},
		{name: "partial five-hour window", unified: true, modify: func(h http.Header) {
			h.Set("Anthropic-Ratelimit-Unified-5h-Reset", strconv.FormatInt(now.Add(time.Hour).Unix(), 10))
		}},
		{name: "unknown weekly status", unified: true, modify: func(h http.Header) { h.Del("Anthropic-Ratelimit-Unified-7d-Status") }},
		{name: "no explicit model rejection", unified: true, modify: func(h http.Header) { h.Del("Anthropic-Ratelimit-Unified-7d_oi-Status") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Anonymized headers from the VM's Fable 429 responses. The shared
			// weekly window is healthy; the model-specific allowance is full.
			h := http.Header{}
			for k, v := range map[string]string{
				"Anthropic-Ratelimit-Unified-Status":                  "rejected",
				"Anthropic-Ratelimit-Unified-7d-Status":               "allowed_warning",
				"Anthropic-Ratelimit-Unified-7d-Utilization":          "0.84",
				"Anthropic-Ratelimit-Unified-7d_oi-Status":            "rejected",
				"Anthropic-Ratelimit-Unified-7d_oi-Utilization":       "1.0",
				"Anthropic-Ratelimit-Unified-Representative-Claim":    "seven_day_overage_included",
				"Anthropic-Ratelimit-Unified-Overage-Status":          "rejected",
				"Anthropic-Ratelimit-Unified-Overage-Disabled-Reason": "org_level_disabled",
				"Anthropic-Ratelimit-Unified-Reset":                   strconv.FormatInt(now.Add(48*time.Hour).Unix(), 10),
				"Anthropic-Ratelimit-Unified-7d_oi-Reset":             strconv.FormatInt(now.Add(48*time.Hour).Unix(), 10),
				"Retry-After": "172800",
			} {
				h.Set(k, v)
			}
			if tc.modify != nil {
				tc.modify(h)
			}
			if got := ClaudeHeadersIndicateUnifiedRateLimitRejection(h); got != tc.unified {
				t.Errorf("credential-wide=%v, want %v", got, tc.unified)
			}
			wait := parseClaudeRateLimitResetWithFuzz(h, now, 0, 0)
			if !tc.unified && wait != nil {
				t.Errorf("model-only rejection must not impose a shared multi-day wait: %v", *wait)
			}
			if tc.unified && wait == nil {
				t.Error("shared/ambiguous rejection lost its retry deadline")
			}
		})
	}
}
