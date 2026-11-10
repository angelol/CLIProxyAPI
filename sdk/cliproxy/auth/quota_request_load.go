package auth

import (
	"context"
	"sync"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type quotaRequestLoadKey struct{}

// Counts actual local request attempts, including affinity hits and streams.
// No session TTL or guessed request cost is needed to release this load.
type quotaRequestLoad struct {
	mu     sync.Mutex
	active map[string]int
}

func (l *quotaRequestLoad) count(id string) int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.active[id]
}

func (l *quotaRequestLoad) begin(id string) func() {
	l.mu.Lock()
	if l.active == nil {
		l.active = make(map[string]int)
	}
	l.active[id]++
	l.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.active[id]--
			if l.active[id] == 0 {
				delete(l.active, id)
			}
		})
	}
}

func usesQuotaDeadline(selector Selector) bool {
	if affinity, ok := selector.(*SessionAffinitySelector); ok {
		selector = affinity.fallback
	}
	_, ok := selector.(*QuotaDeadlineSelector)
	return ok
}

func (m *Manager) withQuotaRequestLoad(ctx context.Context, selector Selector) context.Context {
	if !usesQuotaDeadline(selector) {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, quotaRequestLoadKey{}, &m.quotaLoad)
}

func (m *Manager) beginQuotaRequest(a *Auth, homeMode bool) func() {
	if homeMode || a == nil || a.Provider != "claude" {
		return nil
	}
	m.mu.RLock()
	enabled := usesQuotaDeadline(m.selector)
	m.mu.RUnlock()
	if !enabled {
		return nil
	}
	return m.quotaLoad.begin(a.ID)
}

// Transfer the request lease to the stream. Errors before stream handoff are
// released by the caller; completion and cancellation release it here.
func wrapQuotaLoadStream(ctx context.Context, result *cliproxyexecutor.StreamResult, release func()) *cliproxyexecutor.StreamResult {
	if release == nil {
		return result
	}
	if result == nil || result.Chunks == nil {
		release()
		return result
	}
	if ctx == nil {
		ctx = context.Background()
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer release()
		for {
			select {
			case <-ctx.Done():
				return
			case chunk, ok := <-result.Chunks:
				if !ok {
					return
				}
				select {
				case <-ctx.Done():
					return
				case out <- chunk:
				}
			}
		}
	}()
	copyResult := *result
	copyResult.Chunks = out
	return &copyResult
}
