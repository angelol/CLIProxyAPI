package auth

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestObserveClaudeUsagePreservesStateAndRejectsStalePolls(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	m := NewManager(nil, &QuotaDeadlineSelector{}, nil)
	a := deadlineAuth("a", now, .8, time.Hour)
	a.Metadata = map[string]any{"access_token": "test-token"}
	a.Unavailable = true
	a.StatusMessage = "rate limited"
	a.Quota.Exceeded = true
	a.Quota.NextRecoverAt = now.Add(time.Hour)
	a.ModelStates = map[string]*ModelState{"model": {Unavailable: true, NextRetryAfter: now.Add(time.Hour)}}
	if _, err := m.Register(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	headers := http.Header{"Anthropic-Ratelimit-Unified-7d-Utilization": {"0.1"}}
	if !m.ObserveClaudeUsage(a, headers, now.Add(time.Second)) {
		t.Fatal("fresh poll rejected")
	}
	got, _ := m.GetByID(a.ID)
	if !got.Unavailable || !got.Quota.Exceeded || got.StatusMessage != a.StatusMessage || !got.Quota.NextRecoverAt.Equal(a.Quota.NextRecoverAt) || !got.ModelStates["model"].Unavailable {
		t.Fatal("poll changed runtime availability")
	}
	if quotaSignal(got.Quota.Signals, "Anthropic-Ratelimit-Unified-7d-Utilization") != "0.1" {
		t.Fatal("missing poll observation")
	}
	if m.ObserveClaudeUsage(a, headers, now) {
		t.Fatal("old poll replaced newer feedback")
	}
	wrongToken := a.Clone()
	wrongToken.Metadata["access_token"] = "old-token"
	if m.ObserveClaudeUsage(wrongToken, headers, now.Add(2*time.Second)) {
		t.Fatal("poll for replaced credentials accepted")
	}
	missing := a.Clone()
	missing.ID = "deleted"
	if m.ObserveClaudeUsage(missing, headers, now.Add(2*time.Second)) {
		t.Fatal("poll resurrected removed account")
	}
}
