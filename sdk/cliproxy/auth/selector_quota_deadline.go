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
	quotaProbeInterval     = 20
	quotaAffinityRatio     = 0.8
)

// QuotaDeadlineSelector favors Claude subscription capacity that would otherwise
// expire unused. Its zero value is ready for use. Other providers use round-robin.
// Observations are advisory: this selector never clears the manager's cooldowns.
type QuotaDeadlineSelector struct {
	mu          sync.Mutex
	probes      map[string]uint64
	rotation    RoundRobinSelector
	exploration RoundRobinSelector
	nowFunc     func() time.Time
}

type quotaDeadlinePreparedKey struct{}

func (s *QuotaDeadlineSelector) now() time.Time {
	if s.nowFunc != nil {
		return s.nowFunc()
	}
	return time.Now()
}

// prepare runs once per selection, before any explicit or inferred affinity
// lookup. A binding may survive only within 20% of the best urgency score.
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
	var eligible, unknown, measured []*Auth
	scores := make(map[string]float64, len(available))
	knownScores := make(map[string]bool, len(available))
	var earliest time.Time
	for _, a := range available {
		score, known, blockedUntil := quotaDeadlineScore(a, now)
		if !blockedUntil.IsZero() {
			if earliest.IsZero() || blockedUntil.Before(earliest) {
				earliest = blockedUntil
			}
			continue
		}
		eligible = append(eligible, a)
		scores[a.ID] = score
		knownScores[a.ID] = known
	}
	if len(eligible) == 0 {
		return ctx, nil, NewModelCooldownError(model, provider, earliest.Sub(now))
	}
	var best float64
	for _, a := range highestPriorityAuths(eligible) {
		if !knownScores[a.ID] {
			unknown = append(unknown, a)
		} else {
			measured = append(measured, a)
			best = math.Max(best, scores[a.ID])
		}
	}
	// Probes are real user requests, not background traffic. Even sticky sessions
	// must occasionally visit unmeasured accounts so new/reset accounts can join.
	probe := false
	if len(unknown) > 0 && len(measured) > 0 {
		key := provider + ":" + canonicalModelKey(model)
		s.mu.Lock()
		_, exists := s.probes[key]
		if s.probes == nil || (!exists && len(s.probes) >= 4096) {
			s.probes = make(map[string]uint64)
		}
		s.probes[key] = (s.probes[key] + 1) % quotaProbeInterval
		probe = s.probes[key] == 0
		s.mu.Unlock()
	}
	if len(measured) == 0 || probe {
		a, errPick := s.exploration.Pick(ctx, provider, model, opts, unknown)
		if errPick != nil {
			return ctx, nil, errPick
		}
		return ctx, []*Auth{a}, nil
	}
	preferred := make([]*Auth, 0, len(measured))
	for _, a := range measured {
		if scores[a.ID] >= best*quotaAffinityRatio {
			preferred = append(preferred, a)
		}
	}
	return ctx, preferred, nil
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
	for _, a := range candidates {
		score, _, _ := quotaDeadlineScore(a, now)
		if len(tied) == 0 || score > best {
			best, tied = score, []*Auth{a}
		} else if score == best {
			tied = append(tied, a)
		}
	}
	return s.rotation.Pick(ctx, provider, model, opts, tied)
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
