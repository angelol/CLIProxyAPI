package auth

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestQuotaWeightedHeadroomAndActiveLoad(t *testing.T) {
	for _, mode := range []string{"headroom", "active", "both"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now().Truncate(time.Second)
			q := &QuotaDeadlineSelector{nowFunc: func() time.Time { return now }}
			a, b := deadlineAuth("a", now, .2, 48*time.Hour), deadlineAuth("b", now, .2, 48*time.Hour)
			load := &quotaRequestLoad{}
			if mode != "active" {
				a.Quota.Signals["Anthropic-Ratelimit-Unified-5h-Reset"] = strconv.FormatInt(now.Add(time.Hour).Unix(), 10)
				a.Quota.Signals["Anthropic-Ratelimit-Unified-5h-Utilization"] = "0.75"
			}
			if mode != "headroom" {
				for range 3 {
					defer load.begin("a")()
				}
			}
			ctx := context.WithValue(context.Background(), quotaRequestLoadKey{}, load)
			counts := map[string]int{}
			for range 340 {
				got, err := q.Pick(ctx, "claude", "model", cliproxyexecutor.Options{}, []*Auth{a, b})
				if err != nil {
					t.Fatal(err)
				}
				counts[got.ID]++
			}
			want := 68 // (1/4)/(1+1/4) = 20%.
			if mode == "both" {
				want = 20
			} // (1/16)/(1+1/16).
			if counts["a"] != want || counts["b"] != 340-want {
				t.Fatalf("distribution=%v want a=%d", counts, want)
			}
		})
	}
}

func TestQuotaWeightedChangingScoresAndCrossModelBursts(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	q := &QuotaDeadlineSelector{nowFunc: func() time.Time { return now }}
	a, b := deadlineAuth("a", now, .2, 24*time.Hour), deadlineAuth("b", now, .2, 48*time.Hour)
	counts := map[string]int{}
	for i := range 300 {
		// Never reset credits just because elapsed time or the model changes.
		now = now.Add(time.Millisecond)
		got, err := q.Pick(context.Background(), "mixed", fmt.Sprintf("model-%d", i), cliproxyexecutor.Options{}, []*Auth{b, a})
		if err != nil {
			t.Fatal(err)
		}
		counts[got.ID]++
	}
	if counts["a"] != 200 || counts["b"] != 100 {
		t.Fatalf("lost smooth distribution: %v", counts)
	}
	// Repeated temporary subsets must not accumulate credit on absent accounts.
	for range 100 {
		pickDeadline(t, q, []*Auth{a}, "a")
	}
	counts = map[string]int{}
	for range 30 {
		got, err := q.Pick(context.Background(), "claude", "model", cliproxyexecutor.Options{}, []*Auth{a, b})
		if err != nil {
			t.Fatal(err)
		}
		counts[got.ID]++
	}
	if counts["b"] < 9 || counts["b"] > 11 {
		t.Fatalf("catch-up burst after exclusion: %v", counts)
	}
}

func TestQuotaWeightedConcurrentSessionBinding(t *testing.T) {
	now := time.Now()
	q := &QuotaDeadlineSelector{nowFunc: func() time.Time { return now }}
	s := NewSessionAffinitySelector(q)
	defer s.Stop()
	a, b := deadlineAuth("a", now, .2, 48*time.Hour), deadlineAuth("b", now, .2, 48*time.Hour)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got, err := s.Pick(context.Background(), "claude", "model", deadlineOptions("explicit"), []*Auth{a, b})
			if err != nil || got == nil || got.ID != "a" {
				t.Errorf("concurrent session split: %v %v", got, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	// The first binding used one placement credit; its 99 reuses used none.
	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": {"new"}}}
	got, err := s.Pick(context.Background(), "claude", "model", opts, []*Auth{a, b})
	if err != nil || got.ID != "b" {
		t.Fatalf("affinity hits consumed placement credits: %v %v", got, err)
	}
}

func TestQuotaWeightedHeadroomValidation(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	for _, tc := range []struct {
		usage string
		reset time.Duration
		age   time.Duration
		want  float64
	}{
		{"0.75", time.Hour, 0, .25}, {"0", time.Hour, 0, 1}, {"1", time.Hour, 0, 0},
		{"NaN", time.Hour, 0, 1}, {"+Inf", time.Hour, 0, 1}, {"-1", time.Hour, 0, 1},
		{"bad", time.Hour, 0, 1}, {"0.75", 0, 0, 1}, {"0.75", 9 * 24 * time.Hour, 0, 1},
		{"0.75", time.Hour, quotaObservationMaxAge, 1},
	} {
		a := deadlineAuth("a", now, .2, 48*time.Hour)
		a.Quota.ObservedAt = now.Add(-tc.age)
		a.Quota.Signals["Anthropic-Ratelimit-Unified-5h-Reset"] = strconv.FormatInt(now.Add(tc.reset).Unix(), 10)
		a.Quota.Signals["Anthropic-Ratelimit-Unified-5h-Utilization"] = tc.usage
		if got := quotaFiveHourHeadroom(a, now); got != tc.want {
			t.Fatalf("%+v headroom=%v", tc, got)
		}
	}
}
