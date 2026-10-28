package cliproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	log "github.com/sirupsen/logrus"
)

const claudeUsageURL = "https://api.anthropic.com/api/oauth/usage"
const claudeUsageRefreshInterval = 5 * time.Minute

type claudeUsageFetcher func(context.Context, *config.Config, *coreauth.Auth) (http.Header, error)

func (s *Service) startQuotaRefresh(ctx context.Context) {
	s.quotaRefreshMu.Lock()
	defer s.quotaRefreshMu.Unlock()
	if s.quotaRefreshCancel != nil || s.coreManager == nil {
		return
	}
	ctx, s.quotaRefreshCancel = context.WithCancel(ctx)
	s.quotaRefreshDone = make(chan struct{})
	done := s.quotaRefreshDone
	go func() {
		defer close(done)
		for {
			s.refreshClaudeUsage(ctx, fetchClaudeUsage)
			timer := time.NewTimer(claudeUsageRefreshInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
}

func (s *Service) stopQuotaRefresh() {
	s.quotaRefreshMu.Lock()
	defer s.quotaRefreshMu.Unlock()
	if s.quotaRefreshCancel != nil {
		s.quotaRefreshCancel()
		<-s.quotaRefreshDone
		s.quotaRefreshCancel = nil
	}
}

func (s *Service) quotaRefreshConfig() *config.Config {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	if s.cfg == nil || s.cfg.Home.Enabled || normalizedRoutingRuntimeState(s.cfg).strategy != "quota-deadline" {
		return nil
	}
	return s.cfg
}

func (s *Service) refreshClaudeUsage(ctx context.Context, fetch claudeUsageFetcher) {
	if s.coreManager == nil || s.quotaRefreshConfig() == nil || ctx.Err() != nil {
		return
	}
	for _, a := range s.coreManager.List() {
		cfg := s.quotaRefreshConfig()
		if ctx.Err() != nil || cfg == nil {
			return
		}
		token, _ := a.Metadata["access_token"].(string)
		if a.Provider != "claude" || a.Disabled || a.AuthKind() == coreauth.AuthKindAPIKey || strings.TrimSpace(token) == "" || a.Attributes["base_url"] != "" {
			continue
		}
		observedAt := time.Now()
		headers, err := fetch(ctx, cfg, a)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			// Fetch errors deliberately contain no response bodies or credentials.
			log.WithField("auth", a.ID).WithError(err).Warn("quota-deadline: usage refresh failed; retaining last observation")
			continue
		}
		if s.quotaRefreshConfig() != nil && s.coreManager.ObserveClaudeUsage(a, headers, observedAt) {
			log.WithField("auth", a.ID).Info("quota-deadline: usage refreshed without inference")
		}
	}
}

func fetchClaudeUsage(ctx context.Context, cfg *config.Config, a *coreauth.Auth) (http.Header, error) {
	client := helps.NewUtlsHTTPClient(ctx, cfg, a, 0)
	// Only the fixed provider endpoint may receive the account's token.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return requestClaudeUsage(ctx, client, a)
}

func requestClaudeUsage(ctx context.Context, client *http.Client, a *coreauth.Auth) (http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, claudeUsageURL, nil)
	if err != nil {
		return nil, fmt.Errorf("cannot construct usage request")
	}
	token, _ := a.Metadata["access_token"].(string)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("usage request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("usage endpoint returned HTTP %d", resp.StatusCode)
	}
	const maxBody = 64 * 1024
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		return nil, fmt.Errorf("cannot read bounded usage response")
	}
	return claudeUsageHeaders(body)
}

// The usage endpoint reports percentages; inference headers use fractions.
// Convert only shared windows, never overage or model-specific allowances.
func claudeUsageHeaders(body []byte) (http.Header, error) {
	type window struct {
		Utilization *float64 `json:"utilization"`
		ResetsAt    string   `json:"resets_at"`
	}
	var payload struct {
		FiveHour *window `json:"five_hour"`
		SevenDay *window `json:"seven_day"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return nil, fmt.Errorf("invalid usage response")
	}
	headers := make(http.Header)
	for key, value := range map[string]*window{"5h": payload.FiveHour, "7d": payload.SevenDay} {
		if value == nil {
			continue
		}
		// An unused five-hour session can have no reset time yet.
		if key == "5h" && value.ResetsAt == "" && value.Utilization != nil && *value.Utilization == 0 {
			continue
		}
		reset, err := time.Parse(time.RFC3339Nano, value.ResetsAt)
		if err != nil || value.Utilization == nil || math.IsNaN(*value.Utilization) || math.IsInf(*value.Utilization, 0) || *value.Utilization < 0 {
			return nil, fmt.Errorf("invalid shared usage window")
		}
		prefix := "Anthropic-Ratelimit-Unified-" + key + "-"
		headers.Set(prefix+"Reset", strconv.FormatInt(reset.Unix(), 10))
		headers.Set(prefix+"Utilization", strconv.FormatFloat(*value.Utilization/100, 'f', -1, 64))
		status := "allowed"
		if *value.Utilization >= 100 {
			status = "rejected"
		}
		headers.Set(prefix+"Status", status)
	}
	if len(headers) == 0 {
		return nil, fmt.Errorf("usage response has no shared windows")
	}
	return headers, nil
}
