package management

import "testing"

func TestNormalizeRoutingStrategyQuotaDeadline(t *testing.T) {
	for _, input := range []string{"quota-deadline", " QUOTA-DEADLINE "} {
		got, ok := normalizeRoutingStrategy(input)
		if !ok || got != "quota-deadline" {
			t.Fatalf("normalizeRoutingStrategy(%q) = %q, %v", input, got, ok)
		}
	}
}

func TestNormalizeRoutingStrategyWeightedRoundRobin(t *testing.T) {
	for _, input := range []string{"weighted-round-robin", "weightedroundrobin", "wrr"} {
		got, ok := normalizeRoutingStrategy(input)
		if !ok || got != "weighted-round-robin" {
			t.Fatalf("normalizeRoutingStrategy(%q) = %q, %v; want weighted-round-robin, true", input, got, ok)
		}
	}
}
