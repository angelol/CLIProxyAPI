package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func deadlineAuth(id string, now time.Time, used float64, resetIn time.Duration) *Auth {
	a := &Auth{ID: id, Provider: "claude", Status: StatusActive}
	a.Quota.ObserveResponseHeadersForProvider("claude", http.Header{
		"Anthropic-Ratelimit-Unified-7d-Utilization": {strconv.FormatFloat(used, 'f', -1, 64)},
		"Anthropic-Ratelimit-Unified-7d-Reset":       {strconv.FormatInt(now.Add(resetIn).Unix(), 10)},
	}, now)
	return a
}

func pickDeadline(t *testing.T, selector Selector, auths []*Auth, want string) {
	t.Helper()
	a, err := selector.Pick(context.Background(), "claude", "claude-sonnet", cliproxyexecutor.Options{}, auths)
	if err != nil || a == nil || a.ID != want {
		t.Fatalf("Pick = %v, %v; want %s", a, err, want)
	}
}

func TestQuotaDeadlineScreenshotScenario(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	auths := []*Auth{
		deadlineAuth("later", now, .64, 98*time.Hour),
		deadlineAuth("earlier", now, .21, 67*time.Hour),
		deadlineAuth("middle", now, .78, 81*time.Hour),
	}
	s := &QuotaDeadlineSelector{nowFunc: func() time.Time { return now }}
	for range 50 {
		pickDeadline(t, s, auths, "earlier")
	}
	// Same deadline does not imply equal urgency: remaining capacity matters.
	auths[1] = deadlineAuth("earlier", now, .99, 67*time.Hour)
	pickDeadline(t, s, auths, "later")
}

func TestQuotaDeadlineTelemetryValidation(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	for _, tt := range []struct {
		name, field, value string
		age                time.Duration
		known, blocked     bool
	}{
		{"valid", "", "", 0, true, false},
		{"stale", "", "", quotaObservationMaxAge, false, false},
		{"future observation", "", "", -time.Second, false, false},
		{"missing usage", "7d-Utilization", "", 0, false, false},
		{"nan", "7d-Utilization", "NaN", 0, false, false},
		{"infinity", "7d-Utilization", "+Inf", 0, false, false},
		{"negative", "7d-Utilization", "-0.1", 0, false, false},
		{"bad reset", "7d-Reset", "bad", 0, false, false},
		{"milliseconds", "7d-Reset", strconv.FormatInt(now.Add(time.Hour).UnixMilli(), 10), 0, false, false},
		{"reset passed", "7d-Reset", strconv.FormatInt(now.Unix(), 10), 0, false, false},
		{"exhausted", "7d-Utilization", "1", 0, false, true},
		{"over limit", "7d-Utilization", "1.02", 0, false, true},
		{"rejected", "7d-Status", "rejected", 0, false, true},
		{"warning", "7d-Status", "allowed_warning", 0, true, false},
		{"overage independent", "7d_oi-Status", "rejected", 0, true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a := deadlineAuth("a", now, .2, 2*24*time.Hour)
			a.Quota.ObservedAt = now.Add(-tt.age)
			if tt.field != "" {
				a.Quota.Signals[http.CanonicalHeaderKey("Anthropic-Ratelimit-Unified-"+tt.field)] = tt.value
			}
			_, known, until := quotaDeadlineScore(a, now)
			if known != tt.known || !until.IsZero() != tt.blocked {
				t.Fatalf("known=%v until=%v", known, until)
			}
		})
	}
}

func TestQuotaDeadlineShortWindowAndCooldowns(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	s := &QuotaDeadlineSelector{nowFunc: func() time.Time { return now }}
	a := deadlineAuth("urgent", now, .1, 24*time.Hour)
	b := deadlineAuth("backup", now, .5, 96*time.Hour)
	a.Quota.Signals["Anthropic-Ratelimit-Unified-5h-Utilization"] = "1"
	a.Quota.Signals["Anthropic-Ratelimit-Unified-5h-Reset"] = strconv.FormatInt(now.Add(5*time.Minute).Unix(), 10)
	pickDeadline(t, s, []*Auth{a, b}, "backup")
	_, err := s.Pick(context.Background(), "claude", "model", cliproxyexecutor.Options{}, []*Auth{a})
	var cooldown *modelCooldownError
	if !errors.As(err, &cooldown) || cooldown.resetIn != 5*time.Minute {
		t.Fatalf("expected five minute cooldown, got %v", err)
	}
	now = now.Add(5 * time.Minute)
	pickDeadline(t, s, []*Auth{a, b}, "urgent")
	// A manual quota reset observation cannot override a live runtime cooldown.
	a.Quota.Exceeded, a.Quota.Reason, a.Quota.NextRecoverAt = true, "credential_quota", now.Add(time.Hour)
	pickDeadline(t, s, []*Auth{a, b}, "backup")
	a.Quota.Exceeded = false
	a.ModelStates = map[string]*ModelState{"claude-sonnet": {Unavailable: true, NextRetryAfter: now.Add(time.Hour)}}
	pickDeadline(t, s, []*Auth{a, b}, "backup")
	a.ModelStates = nil
	a.Disabled = true
	pickDeadline(t, s, []*Auth{a, b}, "backup")
}

func TestQuotaDeadlinePriorityWeightAndTies(t *testing.T) {
	now := time.Now()
	s := &QuotaDeadlineSelector{nowFunc: func() time.Time { return now }}
	a := deadlineAuth("a", now, .2, 24*time.Hour)
	b := deadlineAuth("b", now, .2, 24*time.Hour)
	pickDeadline(t, s, []*Auth{b, a}, "a")
	pickDeadline(t, s, []*Auth{b, a}, "b")
	b.Attributes = map[string]string{AttributeWeight: "2"}
	pickDeadline(t, s, []*Auth{a, b}, "b")
	b.Attributes[AttributeWeight] = "0"
	pickDeadline(t, s, []*Auth{a, b}, "a")
	b.Attributes = map[string]string{"priority": "10"}
	b.Quota = deadlineAuth("b", now, .99, 7*24*time.Hour).Quota
	pickDeadline(t, s, []*Auth{a, b}, "b")
	b.Quota.Signals["Anthropic-Ratelimit-Unified-7d-Utilization"] = "1"
	pickDeadline(t, s, []*Auth{a, b}, "a")
}

func TestQuotaDeadlineResetStalenessAndRefresh(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	s := &QuotaDeadlineSelector{nowFunc: func() time.Time { return now }}
	a := deadlineAuth("a", now, .2, time.Minute)
	b := deadlineAuth("b", now, .3, 48*time.Hour)
	pickDeadline(t, s, []*Auth{a, b}, "a")
	now = now.Add(time.Minute)
	// The old window is no longer evidence of available quota, so probe it.
	for range quotaProbeInterval - 1 {
		pickDeadline(t, s, []*Auth{a, b}, "b")
	}
	pickDeadline(t, s, []*Auth{a, b}, "a")
	// A refreshed window re-enters ranking with its new deadline.
	a.Quota = deadlineAuth("a", now, 0, 7*24*time.Hour).Quota
	pickDeadline(t, s, []*Auth{a, b}, "b")
	now = now.Add(quotaObservationMaxAge)
	// Both observations have aged out: sticky or not, explore the whole pool.
	seen := map[string]bool{}
	for range 2 {
		got, err := s.Pick(context.Background(), "claude", "model", cliproxyexecutor.Options{}, []*Auth{a, b})
		if err != nil {
			t.Fatal(err)
		}
		seen[got.ID] = true
	}
	if len(seen) != 2 {
		t.Fatalf("cold pool starved a credential: %v", seen)
	}
}

func TestQuotaDeadlineSubagentCannotInheritUncompetitiveAccount(t *testing.T) {
	now := time.Now()
	s := NewSessionAffinitySelector(&QuotaDeadlineSelector{nowFunc: func() time.Time { return now }})
	defer s.Stop()
	a, b := deadlineAuth("a", now, .1, 24*time.Hour), deadlineAuth("b", now, .5, 48*time.Hour)
	parent := cliproxyexecutor.Options{Headers: http.Header{"X-Task-Id": {"parent"}}}
	got, err := s.Pick(context.Background(), "claude", "model", parent, []*Auth{a, b})
	if err != nil || got.ID != "a" {
		t.Fatalf("parent=%v %v", got, err)
	}
	a.Quota = deadlineAuth("a", now, .99, 24*time.Hour).Quota
	child := cliproxyexecutor.Options{Headers: http.Header{"X-Task-Id": {"child"}, "X-Parent-Task-Id": {"parent"}}}
	got, err = s.Pick(context.Background(), "claude", "model", child, []*Auth{a, b})
	if err != nil || got.ID != "b" {
		t.Fatalf("child inherited stale choice: %v %v", got, err)
	}
}

func TestQuotaDeadlineProbesWithAffinity(t *testing.T) {
	now := time.Now()
	for _, mode := range []string{"none", "explicit", "lcp"} {
		t.Run(mode, func(t *testing.T) {
			q := &QuotaDeadlineSelector{nowFunc: func() time.Time { return now }}
			var s Selector = q
			if mode != "none" {
				affinity := NewSessionAffinitySelector(q)
				defer affinity.Stop()
				s = affinity
			}
			a := deadlineAuth("known", now, .2, 48*time.Hour)
			b, c := &Auth{ID: "unknown-b", Provider: "claude"}, &Auth{ID: "unknown-c", Provider: "claude"}
			counts := map[string]int{}
			for range 40 {
				opts := deadlineOptions(mode)
				got, err := s.Pick(context.Background(), "claude", "claude-sonnet", opts, []*Auth{a, b, c})
				if err != nil {
					t.Fatal(err)
				}
				counts[got.ID]++
			}
			if counts["known"] != 38 || counts[b.ID] != 1 || counts[c.ID] != 1 {
				t.Fatalf("probe distribution: %v", counts)
			}
		})
	}
}

func deadlineOptions(mode string) cliproxyexecutor.Options {
	opts := cliproxyexecutor.Options{Metadata: map[string]any{}}
	if mode == "explicit" {
		opts.Headers = http.Header{"X-Session-Id": {"stable"}}
	}
	if mode == "lcp" {
		opts.SourceFormat = sdktranslator.FormatOpenAI
		opts.OriginalRequest = []byte(`{"messages":[{"role":"system","content":"stable"},{"role":"user","content":"hello"}]}`)
		opts.Metadata[cliproxyexecutor.CallerScopeMetadataKey] = "deadline-test"
	}
	return opts
}

func TestQuotaDeadlineAffinityRebalancesAndKeepsNearTies(t *testing.T) {
	now := time.Now()
	for _, mode := range []string{"explicit", "lcp"} {
		t.Run(mode, func(t *testing.T) {
			q := &QuotaDeadlineSelector{nowFunc: func() time.Time { return now }}
			s := NewSessionAffinitySelector(q)
			defer s.Stop()
			a := deadlineAuth("a", now, .5, 48*time.Hour)
			b := deadlineAuth("b", now, .55, 48*time.Hour)
			pick := func(want string) {
				t.Helper()
				got, err := s.Pick(context.Background(), "claude", "claude-sonnet", deadlineOptions(mode), []*Auth{a, b})
				if err != nil || got.ID != want {
					t.Fatalf("got %v %v want %s", got, err, want)
				}
			}
			pick("a")
			b.Quota = deadlineAuth("b", now, .45, 48*time.Hour).Quota
			pick("a") // b leads by 10%; keep the warm binding.
			b.Quota = deadlineAuth("b", now, .1, 24*time.Hour).Quota
			pick("b") // b is substantially more urgent, even in the same session.
		})
	}
}

func TestQuotaDeadlineFallbackAndConcurrency(t *testing.T) {
	s := &QuotaDeadlineSelector{}
	auths := []*Auth{{ID: "a", Provider: "codex"}, {ID: "b", Provider: "codex"}}
	for i := range 6 {
		a, err := s.Pick(context.Background(), "codex", "model", cliproxyexecutor.Options{}, auths)
		if err != nil || a.ID != auths[i%2].ID {
			t.Fatalf("fallback %v %v", a, err)
		}
	}
	var wg sync.WaitGroup
	var probes atomic.Int64
	auths = []*Auth{deadlineAuth("known", time.Now(), .2, 48*time.Hour), {ID: "unknown", Provider: "claude"}}
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				got, err := s.Pick(context.Background(), "claude", "model", cliproxyexecutor.Options{}, auths)
				if err != nil {
					t.Error(err)
				} else if got.ID == "unknown" {
					probes.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if got := probes.Load(); got != 80 {
		t.Fatalf("concurrent probe count=%d, want 80", got)
	}
}

func TestQuotaDeadlineSimulationExpiresLessUnusedCapacity(t *testing.T) {
	// One unit/request, identical account capacity and demand for both strategies.
	// Account a has 79 units expiring tomorrow, b 79 expiring in four days.
	// 80 requests arrive before a resets. RR strands 39 units; deadline routing
	// spends most requests on a. Feedback follows every request.
	for _, strategy := range []string{"round-robin", "quota-deadline"} {
		t.Run(strategy, func(t *testing.T) {
			now := time.Now().Truncate(time.Second)
			auths := []*Auth{deadlineAuth("a", now, .21, 24*time.Hour), deadlineAuth("b", now, .21, 96*time.Hour)}
			var s Selector = &RoundRobinSelector{}
			if strategy == "quota-deadline" {
				s = &QuotaDeadlineSelector{nowFunc: func() time.Time { return now }}
			}
			used := map[string]int{"a": 21, "b": 21}
			now = now.Add(23 * time.Hour)
			// Both accounts were observed at the beginning of the burst. After
			// that, only the selected account supplies fresh response headers.
			for _, a := range auths {
				a.Quota.ObservedAt = now
			}
			for range 80 {
				pick, err := s.Pick(context.Background(), "claude", "model", cliproxyexecutor.Options{}, auths)
				if err != nil {
					t.Fatal(err)
				}
				used[pick.ID]++
				pick.Quota.ObservedAt = now
				pick.Quota.Signals["Anthropic-Ratelimit-Unified-7d-Utilization"] = fmt.Sprint(float64(used[pick.ID]) / 100)
				// Demand is concentrated in the final hour before the early reset.
				now = now.Add(45 * time.Second)
			}
			waste := 100 - used["a"]
			t.Logf("unused units on early account: %d; allocation a=%d b=%d", waste, used["a"]-21, used["b"]-21)
			if strategy == "round-robin" && waste != 39 {
				t.Fatalf("RR waste %d", waste)
			}
			if strategy == "quota-deadline" && waste >= 39 {
				t.Fatalf("deadline did not improve waste: %d", waste)
			}
		})
	}
}
