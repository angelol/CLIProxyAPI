package cliproxy

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestQuotaRefreshFiveHourRemovalCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name       string
		fiveHour   string
		weeklyUsed int
		wantA      int
	}{
		{"omitted", "", 25, 75},
		{"null", `"five_hour":null,`, 25, 75},
		{"inactive", `"five_hour":{"utilization":0,"resets_at":null},`, 25, 75},
		{"still_limited", `"five_hour":{"utilization":50,"resets_at":%q},`, 25, 60},
		{"weekly_exhausted", "", 100, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC()
			weeklyReset := now.Add(48 * time.Hour).Format(time.RFC3339Nano)
			sessionReset := now.Add(2 * time.Hour).Format(time.RFC3339Nano)
			weighted := &coreauth.QuotaDeadlineSelector{}
			sticky := coreauth.NewSessionAffinitySelector(weighted)
			defer sticky.Stop()
			m := coreauth.NewManager(nil, weighted, nil)
			s := &Service{cfg: &config.Config{}, coreManager: m}
			s.cfg.Routing.Strategy = "quota-deadline"
			for _, id := range []string{"a", "b"} {
				a := &coreauth.Auth{ID: id, Provider: "claude", Status: coreauth.StatusActive, Metadata: map[string]any{"access_token": "test"}}
				used := 75
				five := ""
				if id == "a" {
					used = 25
					five = fmt.Sprintf(`"five_hour":{"utilization":100,"resets_at":%q},`, sessionReset)
				}
				h, err := claudeUsageHeaders([]byte(fmt.Sprintf(`{%s"seven_day":{"utilization":%d,"resets_at":%q}}`, five, used, weeklyReset)))
				if err != nil {
					t.Fatal(err)
				}
				a.Quota.ObserveResponseHeadersForProvider("claude", h, now.Add(-time.Minute))
				if _, err := m.Register(context.Background(), a); err != nil {
					t.Fatal(err)
				}
			}
			a, _ := m.GetByID("a")
			b, _ := m.GetByID("b")
			warm := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": {"warm"}}}
			got, err := sticky.Pick(context.Background(), "claude", "model", warm, []*coreauth.Auth{a, b})
			if err != nil || got == nil || got.ID != "b" {
				t.Fatalf("exhausted five-hour account was selected: got=%v err=%v", got, err)
			}
			s.refreshClaudeUsage(context.Background(), func(_ context.Context, _ *config.Config, a *coreauth.Auth) (http.Header, error) {
				used := 75
				five := ""
				if a.ID == "a" {
					used, five = tc.weeklyUsed, tc.fiveHour
					if strings.Contains(five, "%q") {
						five = fmt.Sprintf(five, sessionReset)
					}
				}
				return claudeUsageHeaders([]byte(fmt.Sprintf(`{%s"seven_day":{"utilization":%d,"resets_at":%q}}`, five, used, weeklyReset)))
			})
			a, _ = m.GetByID("a")
			b, _ = m.GetByID("b")
			if tc.name != "still_limited" {
				for key := range a.Quota.Signals {
					if strings.Contains(strings.ToLower(key), "unified-5h-") {
						t.Fatalf("old five-hour signal survived replacement: %s", key)
					}
				}
			}
			counts := map[string]int{}
			for range 100 {
				got, err := weighted.Pick(context.Background(), "claude", "model", cliproxyexecutor.Options{}, []*coreauth.Auth{a, b})
				if err != nil {
					t.Fatal(err)
				}
				counts[got.ID]++
			}
			if counts["a"] != tc.wantA || counts["b"] != 100-tc.wantA {
				t.Fatalf("new-session distribution=%v; want a=%d b=%d", counts, tc.wantA, 100-tc.wantA)
			}
			got, err = sticky.Pick(context.Background(), "claude", "model", warm, []*coreauth.Auth{a, b})
			if err != nil || got == nil || got.ID != "b" {
				t.Fatalf("quota change broke warm binding: got=%v err=%v", got, err)
			}
		})
	}
}
