package main

import (
	"testing"

	"sage-router/internal/provider"
	"sage-router/internal/store"
)

// AC11 (M2 spec §10) — the load path hydrates the Lifecycle facet from the
// persisted connections.state column. A connection persisted "disabled" must
// load Lifecycle=Disabled and be not-selectable after a restart (the
// regression test for the pre-existing disabled-restart bug). The breaker is
// never persisted — it loads CLOSED regardless of the persisted value.

func TestHydrateConnection_DisabledPersistsAcrossReload(t *testing.T) {
	conn := hydrateConnection(&store.Connection{
		ID: "c-disabled", Provider: "openai", Name: "primary",
		AuthType: "apikey", State: "disabled",
	})
	if got := conn.Lifecycle(); got != provider.LifecycleDisabled {
		t.Errorf("lifecycle = %v, want disabled (persisted-disabled must survive a reload)", got)
	}
	if conn.Selectable("") {
		t.Error("a persisted-disabled connection must not be selectable after a reload")
	}
}

func TestHydrateConnection_IdleLoadsSelectable(t *testing.T) {
	conn := hydrateConnection(&store.Connection{
		ID: "c-idle", Provider: "openai", Name: "primary",
		AuthType: "apikey", State: "idle",
	})
	if got := conn.Lifecycle(); got != provider.LifecycleIdle {
		t.Errorf("lifecycle = %v, want idle", got)
	}
	if got := conn.Breaker(); got != provider.BreakerClosed {
		t.Errorf("breaker = %v, want closed (the breaker facet is never persisted)", got)
	}
	if !conn.Selectable("") {
		t.Error("a persisted-idle connection should be selectable after a reload")
	}
}
