package main

import "testing"

func TestDaemonConfigPreservesEntrypointFlags(t *testing.T) {
	cfg := daemonConfig("0.0.0.0", 42, "state.db", "http://gateway", "combo", "/workspace", 7)
	if cfg.Host != "0.0.0.0" || cfg.Port != 42 || cfg.DBPath != "state.db" || cfg.GatewayURL != "http://gateway" || cfg.Model != "combo" || cfg.Workspace != "/workspace" || cfg.MaxTurns != 7 {
		t.Fatalf("daemonConfig lost a flag value: %+v", cfg)
	}
}
