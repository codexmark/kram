package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/codexmark/kram/internal/cli/daemonclient"
	"github.com/codexmark/kram/internal/cli/statusclient"
)

// comboPickerModel wires a two-combo gateway: a multi-provider "smart" combo
// and a single-provider "solo" one, so both routing branches are reachable.
func comboPickerModel(t *testing.T) Model {
	m := testModel(t)
	m.phase = phaseChat
	m.combo = "smart"
	m.strategyData = statusclient.Status{
		Combos: []statusclient.Combo{
			{ID: "smart", Strategy: "smart", Providers: []string{"a", "b"}},
			{ID: "solo", Strategy: "priority", Providers: []string{"a"}},
		},
		Strategies: []string{"priority", "round-robin", "smart", "quality"},
	}
	return m
}

func TestRoutingPanelOpensOnComboLevel(t *testing.T) {
	m := comboPickerModel(t)
	next, cmd := modelResult(m.handleKey(keyMsg("ctrl+s")))
	if next.active != panelStrategyPicker || next.routePickerLevel != routeLevelCombo || cmd == nil {
		t.Fatalf("ctrl+s active=%v level=%v cmd=%v", next.active, next.routePickerLevel, cmd != nil)
	}
	got := next.renderComboPicker()
	if !strings.Contains(got, "smart") || !strings.Contains(got, "solo") || !strings.Contains(got, "2 providers") {
		t.Fatalf("combo picker did not render both combos with counts: %q", got)
	}
}

func TestComboPickerMultiProviderAdvancesToStrategyLevel(t *testing.T) {
	m := comboPickerModel(t)
	m.daemon = daemonclient.New("http://127.0.0.1:0", "") // never dialed; cmd is not run here
	m.active = panelStrategyPicker
	m.routePickerLevel = routeLevelCombo
	m.syncComboPickerFocus() // focus lands on the active "smart" combo (index 0)

	next, cmd := modelResult(m.handleKey(keyMsg("enter")))
	if next.routePickerLevel != routeLevelStrategy {
		t.Fatalf("selecting a multi-provider combo should advance to the strategy level, got %v", next.routePickerLevel)
	}
	if next.combo != "smart" || cmd == nil {
		t.Fatalf("combo=%q cmd=%v (expected active combo switched + daemon command)", next.combo, cmd != nil)
	}
}

func TestComboPickerSingleProviderSwitchesAndCloses(t *testing.T) {
	m := comboPickerModel(t)
	m.daemon = daemonclient.New("http://127.0.0.1:0", "")
	m.active = panelStrategyPicker
	m.routePickerLevel = routeLevelCombo
	m.comboPickerFocus = 1 // the single-provider "solo" combo

	next, cmd := modelResult(m.handleKey(keyMsg("enter")))
	if next.active != panelNone {
		t.Fatalf("selecting a single-provider combo should close the panel, active=%v", next.active)
	}
	if next.combo != "solo" || cmd == nil {
		t.Fatalf("combo=%q cmd=%v", next.combo, cmd != nil)
	}
	if !strings.Contains(next.strategyNotice, "solo") || !strings.Contains(next.strategyNotice, "single provider") {
		t.Fatalf("expected a single-provider switch notice, got %q", next.strategyNotice)
	}
}

func TestComboPickerSwitchHitsDaemon(t *testing.T) {
	var gotCombo string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/combo" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		gotCombo = body["combo"]
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := comboPickerModel(t)
	m.daemon = daemonclient.New(srv.URL, "")
	m.active = panelStrategyPicker
	m.routePickerLevel = routeLevelCombo
	m.comboPickerFocus = 1 // "solo"

	next, cmd := modelResult(m.handleKey(keyMsg("enter")))
	if cmd == nil {
		t.Fatal("expected a daemon set-combo command")
	}
	// The single-provider path batches the switch with a notice-clear tick;
	// running the batch fires both, and the daemon must receive the switch.
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			if c != nil {
				c()
			}
		}
	}
	if gotCombo != "solo" {
		t.Fatalf("daemon received combo=%q, want solo", gotCombo)
	}
	// A confirmed switch clears any prior picker error.
	confirmed, _ := modelResult(next.Update(comboSetMsg{combo: "solo"}))
	if confirmed.strategyPickerErr != nil {
		t.Fatalf("comboSetMsg success left an error: %v", confirmed.strategyPickerErr)
	}
}

func TestStrategyLevelEscGoesBackToComboLevel(t *testing.T) {
	m := comboPickerModel(t)
	m.active = panelStrategyPicker
	m.routePickerLevel = routeLevelStrategy

	next, _ := modelResult(m.handleKey(keyMsg("esc")))
	if next.active != panelStrategyPicker || next.routePickerLevel != routeLevelCombo {
		t.Fatalf("esc on strategy level should step back to combo level, active=%v level=%v", next.active, next.routePickerLevel)
	}
}

func TestStrategyLevelCtrlSSavesAsDefault(t *testing.T) {
	var request map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(statusclient.Combo{ID: "smart", Strategy: "quality", Providers: []string{"a", "b"}})
	}))
	defer srv.Close()

	m := comboPickerModel(t)
	m.gateway = statusclient.New(srv.URL)
	m.active = panelStrategyPicker
	m.routePickerLevel = routeLevelStrategy
	m.combo = "smart"
	// focus the "quality" strategy (index 3)
	m.strategyPickerFocus = 3

	next, cmd := modelResult(m.handleKey(keyMsg("ctrl+s")))
	if !next.strategySwitching || !next.strategySaving || cmd == nil {
		t.Fatalf("ctrl+s on strategy level should start a save: switching=%v saving=%v cmd=%v", next.strategySwitching, next.strategySaving, cmd != nil)
	}
	msg := cmd()
	applied, clearCmd := modelResult(next.Update(msg))
	if request["persist"] != true || request["make_default"] != true {
		t.Fatalf("save should persist and make default: %v", request)
	}
	if applied.active != panelNone || applied.strategySaving || clearCmd == nil {
		t.Fatalf("after save active=%v saving=%v clear=%v", applied.active, applied.strategySaving, clearCmd != nil)
	}
	if !strings.Contains(applied.strategyNotice, "saved default") {
		t.Fatalf("expected a saved-default notice, got %q", applied.strategyNotice)
	}
}
