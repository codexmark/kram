package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/codexmark/kram/internal/cli/statusclient"
)

func strategyPickerModel(t *testing.T) Model {
	m := testModel(t)
	m.phase = phaseChat
	m.combo = "default"
	m.strategyData = statusclient.Status{
		Combos:     []statusclient.Combo{{ID: "default", Strategy: "priority", Providers: []string{"a", "b"}}},
		Strategies: []string{"priority", "round-robin", "smart", "quality", "fast", "reliable"},
	}
	return m
}

func TestStrategyPickerRendersActiveChoiceAndDescription(t *testing.T) {
	m := strategyPickerModel(t)
	m.active = panelStrategyPicker
	got := m.renderStrategyPicker()
	if !strings.Contains(got, "PRIORITY") || !strings.Contains(got, "ATIVA") || !strings.Contains(got, "ordem declarada") {
		t.Fatalf("picker did not render current choice and description: %q", got)
	}
	if lines := strings.Count(strings.TrimSuffix(got, "\n"), "\n") + 1; lines != m.panelHeight() {
		t.Fatalf("picker lines=%d want=%d", lines, m.panelHeight())
	}
}

func TestStrategyPickerOpensFromShortcutAndRouteBarClick(t *testing.T) {
	m := strategyPickerModel(t)
	next, cmd := modelResult(m.handleKey(keyMsg("ctrl+s")))
	if next.active != panelStrategyPicker || cmd == nil {
		t.Fatalf("ctrl+s active=%v cmd=%v", next.active, cmd != nil)
	}

	m.active = panelNone
	next, cmd = modelResult(m.handleMouse(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 2, Y: 0}))
	if next.active != panelStrategyPicker || cmd == nil {
		t.Fatalf("route-bar click active=%v cmd=%v", next.active, cmd != nil)
	}

	m.active = panelNone
	next, cmd = modelResult(m.handleMouse(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: m.routeBarStrategyWidth() + 2, Y: 0}))
	if next.active != panelNone || cmd != nil {
		t.Fatal("click on the passive attempt area should not open the picker")
	}
}

func TestStrategyPickerKeyboardAppliesRuntimeChoice(t *testing.T) {
	var request map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(statusclient.Combo{ID: "default", Strategy: request["strategy"], Providers: []string{"a", "b"}})
	}))
	defer srv.Close()

	m := strategyPickerModel(t)
	m.gateway = statusclient.New(srv.URL)
	m.active = panelStrategyPicker
	next, cmd := modelResult(m.handleKey(keyMsg("down")))
	if next.strategyPickerFocus != 1 || cmd != nil {
		t.Fatalf("down focus=%d cmd=%v", next.strategyPickerFocus, cmd != nil)
	}
	next, cmd = modelResult(next.handleKey(keyMsg("enter")))
	if !next.strategySwitching || cmd == nil {
		t.Fatalf("enter switching=%v cmd=%v", next.strategySwitching, cmd != nil)
	}
	msg := cmd()
	nextModel, clearCmd := next.Update(msg)
	next = nextModel.(Model)
	if request["combo"] != "default" || request["strategy"] != "round-robin" {
		t.Fatalf("request=%v", request)
	}
	if next.active != panelNone || next.currentCombo().Strategy != "round-robin" || next.strategyNotice == "" || clearCmd == nil {
		t.Fatalf("applied model active=%v combo=%+v notice=%q clear=%v", next.active, next.currentCombo(), next.strategyNotice, clearCmd != nil)
	}
}

func TestStrategyPickerMouseRowAppliesChoice(t *testing.T) {
	m := strategyPickerModel(t)
	m.active = panelStrategyPicker
	panelStart := routeBarHeight + m.viewport.Height + inputHeight
	next, cmd := modelResult(m.handleMouse(tea.MouseMsg{
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
		X: 2, Y: panelStart + 3, // second visible option
	}))
	if next.strategyPickerFocus != 1 || cmd == nil || !next.strategySwitching {
		t.Fatalf("mouse focus=%d cmd=%v switching=%v", next.strategyPickerFocus, cmd != nil, next.strategySwitching)
	}
}

func TestStrategyPickerSurfacesGatewayFailure(t *testing.T) {
	m := strategyPickerModel(t)
	m.active = panelStrategyPicker
	m.strategySwitching = true
	nextModel, cmd := m.Update(strategySetMsg{err: assertiveError("denied")})
	next := nextModel.(Model)
	if next.strategyPickerErr == nil || next.strategySwitching || cmd != nil || !strings.Contains(next.renderStrategyPicker(), "denied") {
		t.Fatalf("failure err=%v switching=%v cmd=%v", next.strategyPickerErr, next.strategySwitching, cmd != nil)
	}
}

type assertiveError string

func (e assertiveError) Error() string { return string(e) }
