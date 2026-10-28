package auth

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

const (
	quotaObservationMaxAge = 15 * time.Minute
	quotaDeadlineFloor     = time.Hour
)

// QuotaDeadlineSelector favors Claude subscription capacity that would otherwise
// expire unused. Its zero value is ready for use. Other providers use round-robin.
// Observations are advisory: this selector never clears the manager's cooldowns.
type QuotaDeadlineSelector struct {
	affinityMu sync.Mutex // Serialize first binding and reuse for concurrent session requests.
	mu         sync.Mutex
	credits    map[string]float64
	rotation   RoundRobinSelector
	nowFunc    func() time.Time
}

type quotaDeadlinePreparedKey struct{}

func (s *QuotaDeadlineSelector) now() time.Time {
	if s.nowFunc != nil {
		return s.nowFunc()
	}
	return time.Now()
}

// prepare runs once per selection, before any explicit or inferred affinity
// lookup. Only genuine unavailability may break an existing warm binding.
func (s *QuotaDeadlineSelector) prepare(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (context.Context, []*Auth, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if ready, _ := ctx.Value(quotaDeadlinePreparedKey{}).(bool); ready {
		return ctx, auths, nil
	}
	ctx = context.WithValue(ctx, quotaDeadlinePreparedKey{}, true)
	if !quotaDeadlinePool(provider, auths) {
		return ctx, auths, nil
	}
	now := s.now()
	available, err := getSelectorAvailableAuthsAcrossPriorities(ctx, positiveWeightAuths(auths), provider, model, now)
	if err != nil {
		return ctx, nil, err
	}
	var eligible []*Auth
	var earliest time.Time
	for _, a := range available {
		_, _, blockedUntil := quotaDeadlineScore(a, now)
		if !blockedUntil.IsZero() {
			if earliest.IsZero() || blockedUntil.Before(earliest) {
				earliest = blockedUntil
			}
			continue
		}
		eligible = append(eligible, a)
	}
	if len(eligible) == 0 {
		return ctx, nil, NewModelCooldownError(model, provider, earliest.Sub(now))
	}
	return ctx, eligible, nil
}

func (s *QuotaDeadlineSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	ctx, candidates, err := s.prepare(ctx, provider, model, opts, auths)
	if err != nil {
		return nil, err
	}
	if !quotaDeadlinePool(provider, candidates) {
		return s.rotation.Pick(ctx, provider, model, opts, candidates)
	}
	// Only new placements reach this fallback. Existing affinity bindings bypass
	// both weighting and credit updates, even when their account is busy.
	now := s.now()
	var measured, unknown []quotaPlacement
	load, _ := ctx.Value(quotaRequestLoadKey{}).(*quotaRequestLoad)
	for _, a := range highestPriorityAuths(candidates) {
		urgency, known, _ := quotaDeadlineScore(a, now)
		base := urgency
		if !known {
			base = float64(authWeight(a))
		}
		headroom := quotaFiveHourHeadroom(a, now)
		active := load.count(a.ID)
		weight := base * headroom / (1 + float64(active))
		if weight <= 0 || math.IsNaN(weight) || math.IsInf(weight, 0) {
			continue
		}
		p := quotaPlacement{auth: a, urgency: urgency, headroom: headroom, active: active, weight: weight}
		if known {
			measured = append(measured, p)
		} else {
			unknown = append(unknown, p)
		}
	}
	known := len(measured) > 0
	if !known {
		measured = unknown
	}
	if len(measured) == 0 {
		return nil, &Error{Code: "auth_unavailable", Message: "no auth available with positive quota weight"}
	}
	picked, share := s.pickWeighted(measured)
	selectorLogEntry(ctx).Infof("quota-deadline: weighted placement | auth=%s provider=%s model=%s quota_known=%t urgency=%.6f five_hour_remaining=%.3f active_requests=%d placement_share=%.4f", picked.auth.ID, provider, model, known, picked.urgency, picked.headroom, picked.active, share)
	return picked.auth, nil
}

type quotaPlacement struct {
	auth                      *Auth
	urgency, headroom, weight float64
	active                    int
}

// Normalized smooth weighted round-robin preserves credit across changing quota
// and load observations. Resetting it whenever a score changed would repeatedly
// pick the highest weight. Credits are shared across models for shared budgets.
func (s *QuotaDeadlineSelector) pickWeighted(candidates []quotaPlacement) (quotaPlacement, float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.credits == nil {
		s.credits = make(map[string]float64)
	}
	var total float64
	for _, p := range candidates {
		total += p.weight
	}
	picked := 0
	for i, p := range candidates {
		s.credits[p.auth.ID] += p.weight / total
		best := candidates[picked].auth.ID
		if s.credits[p.auth.ID] > s.credits[best] || (s.credits[p.auth.ID] == s.credits[best] && p.auth.ID < best) {
			picked = i
		}
	}
	p := candidates[picked]
	s.credits[p.auth.ID] -= 1
	// Removed or temporarily excluded accounts do not earn credit. Bound old
	// debt on return so a changing candidate set cannot cause catch-up bursts.
	for _, p := range candidates {
		s.credits[p.auth.ID] = math.Max(-1, math.Min(1, s.credits[p.auth.ID]))
	}
	if len(s.credits) > maxSmoothWeightedStateEntries {
		present := make(map[string]bool, len(candidates))
		for _, p := range candidates {
			present[p.auth.ID] = true
		}
		for id := range s.credits {
			if !present[id] {
				delete(s.credits, id)
			}
		}
	}
	return p, p.weight / total
}

func quotaFiveHourHeadroom(a *Auth, now time.Time) float64 {
	q := a.Quota
	if q.ObservedAt.IsZero() || q.ObservedAt.After(now) || now.Sub(q.ObservedAt) >= quotaObservationMaxAge {
		return 1
	}
	reset, err := strconv.ParseInt(quotaSignal(q.Signals, "Anthropic-Ratelimit-Unified-5h-Reset"), 10, 64)
	if err != nil || !time.Unix(reset, 0).After(now) || time.Unix(reset, 0).Sub(now) > 8*24*time.Hour {
		return 1
	}
	used, err := strconv.ParseFloat(quotaSignal(q.Signals, "Anthropic-Ratelimit-Unified-5h-Utilization"), 64)
	if err != nil || math.IsNaN(used) || math.IsInf(used, 0) || used < 0 {
		return 1
	}
	return math.Max(0, 1-used)
}

// quotaDeadlineScore uses shared subscription windows only. Overage and
// model-specific claims are not shared budgets; existing model cooldowns remain
// authoritative for those. Weight represents relative subscription capacity.
func quotaDeadlineScore(a *Auth, now time.Time) (score float64, known bool, blockedUntil time.Time) {
	q := a.Quota
	if q.ObservedAt.IsZero() || q.ObservedAt.After(now) || now.Sub(q.ObservedAt) >= quotaObservationMaxAge {
		return 0, false, time.Time{}
	}
	var weeklyRemaining float64
	var weeklyReset time.Time
	for _, window := range []string{"5h", "7d"} {
		prefix := "Anthropic-Ratelimit-Unified-" + window + "-"
		resetUnix, errReset := strconv.ParseInt(quotaSignal(q.Signals, prefix+"Reset"), 10, 64)
		reset := time.Unix(resetUnix, 0)
		if errReset != nil || !reset.After(now) || reset.Sub(now) > 8*24*time.Hour {
			continue
		}
		used, errUsed := strconv.ParseFloat(quotaSignal(q.Signals, prefix+"Utilization"), 64)
		validUsage := errUsed == nil && !math.IsNaN(used) && !math.IsInf(used, 0) && used >= 0
		rejected := strings.EqualFold(quotaSignal(q.Signals, prefix+"Status"), "rejected")
		if rejected || (validUsage && used >= 1) {
			// Reconsider stale observations even if their reset is still far away.
			until := reset
			if staleAt := q.ObservedAt.Add(quotaObservationMaxAge); staleAt.Before(until) {
				until = staleAt
			}
			if until.After(blockedUntil) {
				blockedUntil = until
			}
		}
		if window == "7d" && validUsage && used < 1 {
			weeklyRemaining, weeklyReset = 1-used, reset
		}
	}
	if !blockedUntil.IsZero() || weeklyReset.IsZero() {
		return 0, false, blockedUntil
	}
	deadline := weeklyReset.Sub(now)
	if deadline < quotaDeadlineFloor {
		deadline = quotaDeadlineFloor
	}
	return float64(authWeight(a)) * weeklyRemaining / deadline.Hours(), true, time.Time{}
}

func quotaSignal(signals map[string]string, key string) string {
	if value, ok := signals[http.CanonicalHeaderKey(key)]; ok {
		return strings.TrimSpace(value)
	}
	for k, value := range signals {
		if strings.EqualFold(k, key) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// The manager uses "mixed" even when execution has only Claude candidates.
// Heterogeneous provider pools retain ordinary round-robin semantics.
func quotaDeadlinePool(provider string, auths []*Auth) bool {
	if provider == "claude" {
		return true
	}
	if provider != "mixed" || len(auths) == 0 {
		return false
	}
	for _, a := range auths {
		if a == nil || a.Provider != "claude" {
			return false
		}
	}
	return true
}
