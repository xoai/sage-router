package server

import (
	"testing"

	"sage-router/internal/provider"
	"sage-router/internal/routing"
)

// TestConnStrategyFor (M3 T7 — AC9 plumbing half) pins the routing.Strategy →
// provider.SelectStrategy mapping for all seven routing strategies plus an
// unrecognized value: only the two connection-selection strategies carry a
// non-default mapping.
func TestConnStrategyFor(t *testing.T) {
	tests := []struct {
		strategy routing.Strategy
		want     provider.SelectStrategy
	}{
		{routing.StrategyBalanced, provider.SelectDefault},
		{routing.StrategyFast, provider.SelectDefault},
		{routing.StrategyCheap, provider.SelectDefault},
		{routing.StrategyBest, provider.SelectDefault},
		{routing.StrategyUserOrder, provider.SelectDefault},
		{routing.StrategyP2C, provider.SelectP2C},
		{routing.StrategyResetAware, provider.SelectResetAware},
		{routing.Strategy("__unrecognized__"), provider.SelectDefault},
	}
	for _, tt := range tests {
		if got := connStrategyFor(tt.strategy); got != tt.want {
			t.Errorf("connStrategyFor(%q) = %d, want %d", tt.strategy, got, tt.want)
		}
	}
}
