package cliproxy

import (
	"context"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestQuotaDeadlineRoutingConfigAndHotReload(t *testing.T) {
	service := &Service{coreManager: coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)}
	for i, strategy := range []string{"quota-deadline", "round-robin", "quota-deadline"} {
		cfg := &internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: strategy}}
		state := normalizedRoutingRuntimeState(cfg)
		if state.strategy != strategy {
			t.Fatalf("strategy=%q", state.strategy)
		}
		if !service.applyManagerConfig(context.Background(), configCommit{cfg: cfg, sequence: uint64(i + 1)}) {
			t.Fatal("config application failed")
		}
		_, deadline := service.coreManager.Selector().(*coreauth.QuotaDeadlineSelector)
		if deadline != (strategy == "quota-deadline") {
			t.Fatalf("selector=%T", service.coreManager.Selector())
		}
	}
	cfg := &internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: "quota-deadline", SessionAffinity: true}}
	s := newRoutingSelector(normalizedRoutingRuntimeState(cfg))
	affinity, ok := s.(*coreauth.SessionAffinitySelector)
	if !ok {
		t.Fatalf("selector=%T", s)
	}
	affinity.Stop()
}
