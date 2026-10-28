package auth

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"strings"
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
	rotation RoundRobinSelector
	nowFunc  func() time.Time
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
	// Without a surviving session binding, choose maximum urgency. Rotate ties.
	var best float64
	var tied []*Auth
	now := s.now()
	var unknown []*Auth
	for _, a := range highestPriorityAuths(candidates) {
		score, known, _ := quotaDeadlineScore(a, now)
		if !known {
			unknown = append(unknown, a)
			continue
		}
		if len(tied) == 0 || score > best {
			best, tied = score, []*Auth{a}
		} else if score == best {
			tied = append(tied, a)
		}
	}
	known := len(tied) > 0
	if !known {
		tied = unknown
	}
	a, err := s.rotation.Pick(ctx, provider, model, opts, tied)
	if err == nil && a != nil {
		selectorLogEntry(ctx).Infof("quota-deadline: account selected | auth=%s provider=%s model=%s quota_known=%t urgency=%.6f", a.ID, provider, model, known, best)
	}
	return a, err
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
