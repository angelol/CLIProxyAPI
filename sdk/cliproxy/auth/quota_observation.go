package auth

import (
	"net/http"
	"time"
)

// ObserveClaudeUsage installs a background usage observation without changing
// credentials, cooldowns, model states, or session bindings. observedAt is the
// request start time so a slow poll cannot replace newer inference feedback.
func (m *Manager) ObserveClaudeUsage(expected *Auth, headers http.Header, observedAt time.Time) bool {
	if m == nil || expected == nil || expected.Provider != "claude" || observedAt.IsZero() {
		return false
	}
	token, _ := expected.Metadata["access_token"].(string)
	if token == "" {
		return false
	}
	m.mu.Lock()
	current := m.auths[expected.ID]
	if current == nil || current.Disabled || current.Provider != "claude" || current.Quota.ObservedAt.After(observedAt) {
		m.mu.Unlock()
		return false
	}
	currentToken, _ := current.Metadata["access_token"].(string)
	if token != currentToken || !current.Quota.ObserveResponseHeadersForProvider("claude", headers, observedAt) {
		m.mu.Unlock()
		return false
	}
	current.Generation++
	snapshot := current.Clone()
	m.mu.Unlock()
	if m.scheduler != nil {
		m.scheduler.upsertAuth(snapshot)
	}
	return true
}
