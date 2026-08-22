package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/codexmark/kram/internal/cli/daemonclient"
	"github.com/codexmark/kram/internal/cli/statusclient"
	"github.com/codexmark/kram/internal/credentials"
	"github.com/codexmark/kram/internal/customprovider"
	"github.com/codexmark/kram/internal/oauthflow"
	"github.com/codexmark/kram/internal/openai"
	"github.com/codexmark/kram/internal/providercatalog"
	"github.com/codexmark/kram/internal/providerping"
	"github.com/codexmark/kram/internal/toolsettings"
)

func testModel(t *testing.T) Model {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := New(nil, nil, "session-1", "combo", t.TempDir(), false, WizardResult{BootSplashShown: true})
	next, cmd := m.Update(tea.WindowSizeMsg{Width: 100, Height: 36})
	if cmd != nil {
		t.Fatal("window resize unexpectedly returned a command")
	}
	return next.(Model)
}

func TestAllPrimaryViewsRenderTheirState(t *testing.T) {
	m := testModel(t)
	m.sessionList = []daemonclient.Session{{ID: "s", Title: "sessão", UpdatedAt: time.Now().Unix()}}
	m.strategyData = statusclient.Status{
		Combos:    []statusclient.Combo{{ID: "combo", Strategy: "smart", Providers: []string{"p1", "p2"}}},
		Providers: []statusclient.Provider{{ID: "p1", Stats: statusclient.ProviderStats{Requests: 2, SuccessRate: 1, AvgLatencyMS: 8}}, {ID: "p2", BreakerOpen: true}},
	}
	m.contextData = daemonclient.ContextUsage{Used: 80, Budget: 100, Categories: []daemonclient.ContextCategory{{Name: "messages", Tokens: 50}, {Name: "mystery", Tokens: 30}, {Name: "free", Tokens: 0}}}
	m.haveContext = true
	m.routeTrace = daemonclient.RouteTrace{Combo: "combo", Strategy: "smart", Calls: []daemonclient.RouteCall{{Index: 1, Attempts: []openai.AttemptInfo{{Provider: "p1", Outcome: "success", LatencyMS: 8}}}}}
	m.messages = []chatMessage{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "**world**", Notices: []string{"note"}, ToolActivity: []daemonclient.ToolActivity{{Name: "read", Args: strings.Repeat("a", 70), Running: true}, {Name: "write", OK: true}, {Name: "bad", Running: false, OK: false}}},
	}
	m.lastUsage.TotalTokens = 123

	for _, active := range []panel{panelNone, panelStrategy, panelContext, panelRoute} {
		m.phase, m.active = phaseChat, active
		if got := m.View(); got == "" {
			t.Fatalf("empty chat view for panel %d", active)
		}
	}

	m.question = &pendingQuestion{id: "q", question: "choose", options: []string{"a", "b"}}
	if !strings.Contains(m.View(), "choose") {
		t.Fatal("question missing")
	}
	m.question = nil
	m.approval = &pendingApproval{id: "a", tool: "bash", subject: "rm x", options: []string{"once", "deny"}}
	if !strings.Contains(m.View(), "bash") {
		t.Fatal("approval missing")
	}
	m.approval = nil

	for _, p := range []phase{phasePicker, phaseAccounts, phaseTools, phaseWizardToolsPreset, phaseWizardSystemCheck, phaseWizardSummary} {
		m.phase = p
		if got := m.View(); got == "" {
			t.Fatalf("empty view for phase %d", p)
		}
	}

	w := newWizardModel(t.TempDir(), false)
	w.ready, w.width, w.height = true, 100, 36
	for _, p := range []phase{phaseSplash, phaseWizardEnvironment, phaseWizardProjects, phaseWizardRouting, phaseWizardPermissions} {
		w.phase = p
		if got := w.View(); got == "" {
			t.Fatalf("empty wizard view for phase %d", p)
		}
	}
}

func TestViewsCoverLoadingErrorsAndEditors(t *testing.T) {
	m := testModel(t)
	m.phase = phasePicker
	m.pickerErr, m.pickerBusy = errors.New("offline"), true
	if got := m.View(); !strings.Contains(got, "offline") {
		t.Fatalf("picker error view = %q", got)
	}
	m.pickerBusy, m.titling = false, true
	if got := m.View(); !strings.Contains(got, "título") {
		t.Fatalf("picker title editor missing from %q", got)
	}

	m.phase = phaseAccounts
	m.accountsPings = map[string]providerping.Result{"ANTHROPIC_API_KEY": {Status: providerping.StatusDown, Detail: "bad"}}
	m.accountsStatus = "status"
	if got := m.View(); !strings.Contains(got, "bad") || !strings.Contains(got, "status") {
		t.Fatalf("account ping/status view = %q", got)
	}
	m.accountsEditing = true
	if got := m.View(); !strings.Contains(got, "API key") {
		t.Fatalf("account key editor missing from %q", got)
	}
	m.accountsEditing, m.accountsOAuthPending, m.accountsOAuthURL = false, true, "http://auth"
	if got := m.View(); !strings.Contains(got, "http://auth") {
		t.Fatalf("OAuth URL missing from %q", got)
	}
	m.accountsOAuthPending, m.accountsAddingCustom = false, true
	m.customFormInputs = newCustomProviderFormInputs()
	if got := m.View(); !strings.Contains(got, customFormLabels[0]) {
		t.Fatalf("custom provider form missing from %q", got)
	}

	m.phase, m.toolsLoading, m.toolsErr = phaseTools, true, errors.New("tools")
	if got := m.View(); !strings.Contains(got, "tools") {
		t.Fatalf("tools error view = %q", got)
	}
	m.toolsLoading = false
	m.toolsList = []daemonclient.ToolInfo{{Name: "z", Description: strings.Repeat("z", 70)}}
	m.skillsList = []daemonclient.Skill{{Name: "a", Description: "skill"}}
	if got := m.View(); !strings.Contains(got, "z") || !strings.Contains(got, "a") {
		t.Fatalf("tools and skills missing from %q", got)
	}

	m.phase = phaseChat
	m.strategyErr = errors.New("gateway")
	if got := m.renderStrategyPanel(); !strings.Contains(got, "gateway") {
		t.Fatalf("strategy error panel = %q", got)
	}
	m.strategyErr = nil
	m.strategyData = statusclient.Status{}
	if got := m.renderStrategyPanel(); !strings.Contains(got, "nenhum combo") {
		t.Fatalf("empty strategy panel = %q", got)
	}
	m.contextErr = errors.New("context")
	if got := m.renderContextPanel(); !strings.Contains(got, "context") {
		t.Fatalf("context error panel = %q", got)
	}
	m.contextErr = nil
	m.haveContext = false
	if got := m.renderContextPanel(); !strings.Contains(got, "carregando") {
		t.Fatalf("context loading panel = %q", got)
	}
}

func TestQuestionApprovalAndChatKeys(t *testing.T) {
	m := testModel(t)
	m.question = &pendingQuestion{id: "q", question: "?", options: []string{"one", "two"}}
	m, cmd := modelResult(m.handleQuestionKey(keyMsg("down")))
	if m.question.cursor != 1 || cmd != nil {
		t.Fatalf("question down = cursor %d cmd=%v", m.question.cursor, cmd != nil)
	}
	m, cmd = modelResult(m.handleQuestionKey(keyMsg("up")))
	if m.question.cursor != 0 || cmd != nil {
		t.Fatalf("question up = cursor %d cmd=%v", m.question.cursor, cmd != nil)
	}
	m, cmd = modelResult(m.handleQuestionKey(keyMsg("enter")))
	if m.question != nil || cmd == nil {
		t.Fatal("option question not submitted")
	}

	m.question = &pendingQuestion{id: "free", question: "?"}
	m.questionInput = textinput.New()
	m.questionInput.SetValue(" answer ")
	if got := m.renderQuestion(); !strings.Contains(got, "answer") {
		t.Fatalf("free-form answer not rendered: %q", got)
	}
	m, cmd = modelResult(m.handleQuestionKey(keyMsg("enter")))
	if m.question != nil || cmd == nil {
		t.Fatal("free question not submitted")
	}

	m.approval = &pendingApproval{id: "a", tool: "bash", options: []string{"once", "always", "deny"}}
	m, cmd = modelResult(m.handleApprovalKey(keyMsg("down")))
	if m.approval.cursor != 1 || cmd != nil {
		t.Fatalf("approval down = cursor %d cmd=%v", m.approval.cursor, cmd != nil)
	}
	m, cmd = modelResult(m.handleApprovalKey(keyMsg("up")))
	if m.approval.cursor != 0 || cmd != nil {
		t.Fatalf("approval up = cursor %d cmd=%v", m.approval.cursor, cmd != nil)
	}
	m, cmd = modelResult(m.handleApprovalKey(keyMsg("enter")))
	if m.approval != nil || cmd == nil {
		t.Fatal("approval not submitted")
	}

	m.strategyData.Combos = []statusclient.Combo{{ID: "combo", Providers: []string{"a", "b"}}}
	m, cmd = modelResult(m.handleKey(keyMsg("ctrl+p")))
	if m.active != panelStrategy || cmd == nil {
		t.Fatal("ctrl+p did not open and refresh strategy panel")
	}
	m, cmd = modelResult(m.handleKey(keyMsg("down")))
	if m.strategyFocus != 1 || cmd != nil {
		t.Fatal("strategy down did not focus second provider")
	}
	m, cmd = modelResult(m.handleKey(keyMsg("up")))
	if m.strategyFocus != 0 || cmd != nil {
		t.Fatal("strategy up did not focus first provider")
	}
	m, cmd = modelResult(m.handleKey(keyMsg("esc")))
	if m.active != panelNone || cmd != nil {
		t.Fatal("escape did not close strategy panel")
	}
	m, cmd = modelResult(m.handleKey(keyMsg("ctrl+t")))
	if m.active != panelContext || cmd == nil {
		t.Fatal("ctrl+t did not open and refresh context panel")
	}
	m, cmd = modelResult(m.handleKey(keyMsg("ctrl+t")))
	if m.active != panelNone || cmd != nil {
		t.Fatal("second ctrl+t did not close context panel")
	}
	m, cmd = modelResult(m.handleKey(keyMsg("ctrl+r")))
	if m.active != panelRoute || cmd != nil {
		t.Fatal("ctrl+r did not open route panel")
	}
	m, cmd = modelResult(m.handleKey(keyMsg("enter")))
	if m.active != panelNone || cmd != nil {
		t.Fatal("enter did not close route panel")
	}
	m.input.SetValue("send me")
	m, cmd = modelResult(m.handleKey(keyMsg("enter")))
	if !m.waiting || cmd == nil {
		t.Fatal("message not submitted")
	}
	m.input.SetValue("ignored")
	beforeMessages := len(m.messages)
	m, cmd = modelResult(m.submit())
	if cmd != nil || len(m.messages) != beforeMessages {
		t.Fatal("submit while waiting should be ignored")
	}
}

func modelResult(model tea.Model, cmd tea.Cmd) (Model, tea.Cmd) { return model.(Model), cmd }

func TestPickerAccountsToolsAndWizardKeys(t *testing.T) {
	t.Run("picker navigation and destinations", func(t *testing.T) {
		m := testModel(t)
		m.phase = phasePicker
		m.sessionList = []daemonclient.Session{{ID: "one"}, {ID: "two"}}
		for _, key := range []string{"down", "down", "down", "up"} {
			var cmd tea.Cmd
			m, cmd = modelResult(m.handlePickerKey(keyMsg(key)))
			if cmd != nil {
				t.Fatalf("navigation %q unexpectedly returned command", key)
			}
		}
		if m.pickerCursor != 1 {
			t.Fatalf("bounded picker cursor = %d, want 1", m.pickerCursor)
		}
		m, cmd := modelResult(m.handlePickerKey(keyMsg("enter")))
		if m.phase != phaseChat || m.sessionID != "one" || cmd == nil {
			t.Fatalf("existing session selection = phase %d session %q cmd=%v", m.phase, m.sessionID, cmd != nil)
		}

		m.phase, m.pickerCursor = phasePicker, 0
		m, cmd = modelResult(m.handlePickerKey(keyMsg("enter")))
		if !m.titling || cmd != nil {
			t.Fatal("new-session row did not open title editor")
		}
		m, cmd = modelResult(m.handlePickerKey(keyMsg("x")))
		if m.newSessionText.Value() != "x" || cmd == nil {
			t.Fatalf("title input = %q cmd=%v", m.newSessionText.Value(), cmd != nil)
		}
		m, cmd = modelResult(m.handlePickerKey(keyMsg("esc")))
		if m.titling || cmd != nil {
			t.Fatal("escape did not close title editor")
		}
		m, cmd = modelResult(m.handlePickerKey(keyMsg("a")))
		if m.phase != phaseAccounts || !m.accountsPinging || cmd == nil {
			t.Fatal("accounts shortcut did not start account verification")
		}
		m.phase = phasePicker
		m, cmd = modelResult(m.handlePickerKey(keyMsg("f")))
		if m.phase != phaseTools || !m.toolsLoading || cmd == nil {
			t.Fatal("tools shortcut did not start tool loading")
		}
	})

	t.Run("account and custom form navigation", func(t *testing.T) {
		m := testModel(t)
		m.phase = phaseAccounts
		for i, account := range providercatalog.Accounts {
			if !account.OAuthOnly {
				m.accountsCursor = i
				break
			}
		}
		editableCursor := m.accountsCursor
		m, cmd := modelResult(m.handleAccountsKey(keyMsg("down")))
		if m.accountsCursor != editableCursor+1 || cmd != nil {
			t.Fatalf("accounts down = cursor %d cmd=%v", m.accountsCursor, cmd != nil)
		}
		m, cmd = modelResult(m.handleAccountsKey(keyMsg("up")))
		if m.accountsCursor != editableCursor || cmd != nil {
			t.Fatalf("accounts up = cursor %d cmd=%v", m.accountsCursor, cmd != nil)
		}
		m, cmd = modelResult(m.handleAccountsKey(keyMsg("enter")))
		if !m.accountsEditing || cmd == nil {
			t.Fatal("account enter did not open credential editor")
		}
		m, cmd = modelResult(m.handleAccountsKey(keyMsg("esc")))
		if m.accountsEditing || cmd != nil {
			t.Fatal("credential editor escape did not close editor")
		}
		m, cmd = modelResult(m.handleAccountsKey(keyMsg("r")))
		if !m.accountsPinging || cmd == nil {
			t.Fatal("account refresh did not start verification")
		}
		_, _, addRow, _ := m.accountsRowCounts()
		m.accountsCursor = addRow
		m, cmd = modelResult(m.handleAccountsKey(keyMsg("enter")))
		if !m.accountsAddingCustom || len(m.customFormInputs) != len(customFormLabels) || cmd == nil {
			t.Fatal("add-provider row did not open custom form")
		}
		m, cmd = modelResult(m.handleAccountsKey(keyMsg("tab")))
		if m.customFormCursor != 1 || cmd == nil {
			t.Fatalf("custom form tab focus = %d", m.customFormCursor)
		}
		m, cmd = modelResult(m.handleAccountsKey(keyMsg("shift+tab")))
		if m.customFormCursor != 0 || cmd == nil {
			t.Fatalf("custom form reverse-tab focus = %d", m.customFormCursor)
		}
		m, cmd = modelResult(m.handleAccountsKey(keyMsg("x")))
		if m.customFormInputs[0].Value() != "x" || cmd == nil {
			t.Fatalf("custom name input = %q cmd=%v", m.customFormInputs[0].Value(), cmd != nil)
		}
		m, cmd = modelResult(m.handleAccountsKey(keyMsg("esc")))
		if m.accountsAddingCustom || cmd != nil {
			t.Fatal("custom form escape did not close form")
		}
	})

	t.Run("tool toggles persist observable state", func(t *testing.T) {
		m := testModel(t)
		m.phase = phaseTools
		m.toolsList = []daemonclient.ToolInfo{{Name: "b"}, {Name: "a"}}
		m.skillsList = []daemonclient.Skill{{Name: "s"}}
		m, cmd := modelResult(m.handleToolsKey(keyMsg("down")))
		if m.toolsCursor != 1 || cmd != nil {
			t.Fatalf("tools down = cursor %d cmd=%v", m.toolsCursor, cmd != nil)
		}
		m, cmd = modelResult(m.handleToolsKey(keyMsg("up")))
		if m.toolsCursor != 0 || cmd != nil {
			t.Fatalf("tools up = cursor %d cmd=%v", m.toolsCursor, cmd != nil)
		}
		item := m.toolToggleItems()[0].name
		m, cmd = modelResult(m.handleToolsKey(keyMsg("enter")))
		if !m.toolSettings.IsDisabled(item) || cmd == nil || !strings.Contains(m.toolsStatus, "desligado") {
			t.Fatalf("tool toggle failed for %q: disabled=%v status=%q cmd=%v", item, m.toolSettings.IsDisabled(item), m.toolsStatus, cmd != nil)
		}
		m, cmd = modelResult(m.handleToolsKey(keyMsg(" ")))
		if m.toolSettings.IsDisabled(item) || cmd == nil || !strings.Contains(m.toolsStatus, "ligado") {
			t.Fatalf("second tool toggle failed for %q", item)
		}
		m, cmd = modelResult(m.handleToolsKey(keyMsg("d")))
		if cmd == nil || !strings.Contains(m.toolsStatus, "desligados") {
			t.Fatal("disable-all did not persist or schedule daemon sync")
		}
		m, cmd = modelResult(m.handleToolsKey(keyMsg("a")))
		if cmd == nil || !strings.Contains(m.toolsStatus, "ligados") {
			t.Fatal("enable-all did not persist or schedule daemon sync")
		}
		m, cmd = modelResult(m.handleToolsKey(keyMsg("esc")))
		if m.phase != phasePicker || cmd != nil {
			t.Fatal("tools escape did not return to picker")
		}
	})

	t.Run("wizard choices advance and retain selections", func(t *testing.T) {
		w := newWizardModel(t.TempDir(), false)
		initialPhase := w.phase
		w, cmd := modelResult(w.handleWizardEnvironmentKey(keyMsg("x")))
		if w.phase != initialPhase || cmd != nil {
			t.Fatal("irrelevant environment key mutated wizard")
		}
		w, cmd = modelResult(w.handleWizardEnvironmentKey(keyMsg("enter")))
		if w.phase != phaseWizardProjects || cmd == nil {
			t.Fatal("environment enter did not advance to projects")
		}
		root, workspace := t.TempDir(), filepath.Join(t.TempDir(), "ws")
		w.wizardProjectsRootInput.SetValue(root)
		w.wizardWorkspaceInput.SetValue(workspace)
		w, cmd = modelResult(w.handleWizardProjectsKey(keyMsg("tab")))
		if w.wizardProjectsField != 1 || cmd != nil {
			t.Fatal("projects tab did not select workspace")
		}
		w, cmd = modelResult(w.handleWizardProjectsKey(keyMsg("tab")))
		if w.wizardProjectsField != 0 || cmd != nil {
			t.Fatal("projects second tab did not select root")
		}
		beforeRoot := w.wizardProjectsRootInput.Value()
		w, cmd = modelResult(w.handleWizardProjectsKey(keyMsg("x")))
		if w.wizardProjectsRootInput.Value() == beforeRoot || !strings.Contains(w.wizardProjectsRootInput.Value(), "x") {
			t.Fatal("projects text input did not receive key")
		}
		// Restore the valid root after proving input routing.
		w.wizardProjectsRootInput.SetValue(root)
		w, cmd = modelResult(w.handleWizardProjectsKey(keyMsg("enter")))
		if w.phase != phaseAccounts || w.wizardChosenWorkspace != workspace || cmd == nil {
			t.Fatalf("projects submit = phase %d workspace %q cmd=%v", w.phase, w.wizardChosenWorkspace, cmd != nil)
		}
		w.phase = phaseWizardRouting
		w, cmd = modelResult(w.handleWizardRoutingKey(keyMsg("down")))
		if w.wizardRoutingCursor != 1 || cmd != nil {
			t.Fatal("routing down did not select Smart")
		}
		w, cmd = modelResult(w.handleWizardRoutingKey(keyMsg("enter")))
		if w.phase != phaseWizardPermissions || w.wizardChosenStrategy != "smart" || cmd != nil {
			t.Fatal("routing enter did not retain Smart")
		}
		w, cmd = modelResult(w.handleWizardPermissionsKey(keyMsg("down")))
		if w.wizardPermCursor != 1 || cmd != nil {
			t.Fatal("permissions down did not select Strict")
		}
		w, cmd = modelResult(w.handleWizardPermissionsKey(keyMsg("enter")))
		if !w.wizardDone || w.wizardChosenPermPreset != "strict" || cmd == nil {
			t.Fatal("permissions enter did not complete wizard with Strict")
		}
	})

	t.Run("wizard tool preset and system check advance", func(t *testing.T) {
		m := testModel(t)
		m.phase = phaseWizardToolsPreset
		m.toolsList = []daemonclient.ToolInfo{{Name: "read_file"}, {Name: "bash"}}
		m, cmd := modelResult(m.handleWizardToolsPresetKey(keyMsg("down")))
		if m.wizardToolsPresetCursor != 1 || cmd != nil {
			t.Fatal("preset down did not select Minimal")
		}
		m, cmd = modelResult(m.handleWizardToolsPresetKey(keyMsg("up")))
		if m.wizardToolsPresetCursor != 0 || cmd != nil {
			t.Fatal("preset up did not restore Recommended")
		}
		m, cmd = modelResult(m.handleWizardToolsPresetKey(keyMsg("enter")))
		if m.wizardChosenToolsPreset != "recommended" || !m.wizardToolSettingsPending || cmd == nil {
			t.Fatal("recommended preset did not schedule settings sync")
		}
		m.wizardToolsPresetCursor = 2
		m.wizardToolSettingsPending = false
		m, cmd = modelResult(m.handleWizardToolsPresetKey(keyMsg("enter")))
		if m.phase != phaseTools || m.wizardChosenToolsPreset != "custom" || cmd != nil {
			t.Fatal("custom preset did not open individual tool screen")
		}
		m, cmd = modelResult(m.handleWizardSystemCheckKey(keyMsg("enter")))
		if m.phase != phaseWizardSummary || m.wizardStep != 8 || cmd != nil {
			t.Fatal("system check enter did not advance to summary")
		}
	})
}

func TestUpdateMessageMatrix(t *testing.T) {
	m := testModel(t)
	m.pickerBusy = true
	next, cmd := m.Update(sessionsListMsg{sessions: []daemonclient.Session{{ID: "x"}}})
	m = next.(Model)
	if m.pickerBusy || len(m.sessionList) != 1 || m.sessionList[0].ID != "x" || cmd != nil {
		t.Fatalf("sessions success state = %+v busy=%v cmd=%v", m.sessionList, m.pickerBusy, cmd != nil)
	}
	next, cmd = m.Update(sessionsListMsg{err: errors.New("list")})
	m = next.(Model)
	if m.pickerErr == nil || m.pickerErr.Error() != "list" || cmd != nil {
		t.Fatalf("sessions error state = %v cmd=%v", m.pickerErr, cmd != nil)
	}

	next, cmd = m.Update(sessionCreatedMsg{err: errors.New("create")})
	m = next.(Model)
	if m.pickerErr == nil || m.pickerErr.Error() != "create" || cmd != nil {
		t.Fatalf("create error state = %v cmd=%v", m.pickerErr, cmd != nil)
	}
	next, cmd = m.Update(sessionCreatedMsg{session: daemonclient.Session{ID: "new"}})
	m = next.(Model)
	if m.sessionID != "new" || m.phase != phaseChat || cmd == nil {
		t.Fatalf("create success = session %q phase %d cmd=%v", m.sessionID, m.phase, cmd != nil)
	}

	next, cmd = m.Update(historyLoadedMsg{err: errors.New("history")})
	m = next.(Model)
	if m.err == nil || m.err.Error() != "history" || cmd != nil {
		t.Fatalf("history error state = %v cmd=%v", m.err, cmd != nil)
	}
	next, cmd = m.Update(historyLoadedMsg{messages: []daemonclient.Message{{Role: "user", Content: "hi", Provider: "p"}}})
	m = next.(Model)
	if len(m.messages) != 1 || m.messages[0].Content != "hi" || m.messages[0].Provider != "p" || cmd != nil {
		t.Fatalf("history success state = %+v cmd=%v", m.messages, cmd != nil)
	}

	m.waiting = true
	m.messages = append(m.messages, chatMessage{Role: "assistant", streaming: true})
	next, cmd = m.Update(streamStartMsg{err: errors.New("stream")})
	m = next.(Model)
	if m.waiting || m.err == nil || m.err.Error() != "stream" || m.messages[len(m.messages)-1].streaming || cmd != nil {
		t.Fatalf("stream start error = waiting %v err %v messages %+v cmd=%v", m.waiting, m.err, m.messages, cmd != nil)
	}

	next, cmd = m.Update(statusResultMsg{err: errors.New("status")})
	m = next.(Model)
	if m.strategyErr == nil || m.strategyErr.Error() != "status" || cmd != nil {
		t.Fatalf("status error state = %v cmd=%v", m.strategyErr, cmd != nil)
	}
	next, cmd = m.Update(statusResultMsg{status: statusclient.Status{Combos: []statusclient.Combo{{ID: "x"}}}})
	m = next.(Model)
	if m.strategyErr != nil || len(m.strategyData.Combos) != 1 || m.strategyData.Combos[0].ID != "x" || cmd != nil {
		t.Fatalf("status success state = %+v err=%v cmd=%v", m.strategyData, m.strategyErr, cmd != nil)
	}

	next, cmd = m.Update(contextResultMsg{err: errors.New("ctx")})
	m = next.(Model)
	if m.contextErr == nil || m.contextErr.Error() != "ctx" || cmd != nil {
		t.Fatalf("context error state = %v cmd=%v", m.contextErr, cmd != nil)
	}
	next, cmd = m.Update(contextResultMsg{usage: daemonclient.ContextUsage{Budget: 10, Used: 2}})
	m = next.(Model)
	if m.contextErr != nil || !m.haveContext || m.contextData.Budget != 10 || m.contextData.Used != 2 || cmd != nil {
		t.Fatalf("context success state = %+v have=%v err=%v cmd=%v", m.contextData, m.haveContext, m.contextErr, cmd != nil)
	}

	m.accountsPinging = true
	next, cmd = m.Update(pingResultsMsg{results: map[string]providerping.Result{"key": {Status: providerping.StatusOK}}})
	m = next.(Model)
	if m.accountsPinging || m.accountsPings["key"].Status != providerping.StatusOK || cmd != nil {
		t.Fatalf("ping state = %+v pinging=%v cmd=%v", m.accountsPings, m.accountsPinging, cmd != nil)
	}
	m.accountsOAuthPending = true
	next, cmd = m.Update(oauthURLMsg{err: errors.New("oauth")})
	m = next.(Model)
	if m.accountsOAuthPending || !strings.Contains(m.accountsStatus, "oauth") || cmd != nil {
		t.Fatalf("OAuth URL error = pending %v status %q cmd=%v", m.accountsOAuthPending, m.accountsStatus, cmd != nil)
	}
	m.accountsOAuthPending = true
	next, cmd = m.Update(oauthResultMsg{err: errors.New("oauth result")})
	m = next.(Model)
	if m.accountsOAuthPending || !strings.Contains(m.accountsStatus, "oauth result") || cmd != nil {
		t.Fatalf("OAuth result error = pending %v status %q cmd=%v", m.accountsOAuthPending, m.accountsStatus, cmd != nil)
	}

	m.toolsLoading = true
	next, cmd = m.Update(toolsListMsg{err: errors.New("tools")})
	m = next.(Model)
	if m.toolsLoading || m.toolsErr == nil || m.toolsErr.Error() != "tools" || cmd != nil {
		t.Fatalf("tools error = loading %v err %v cmd=%v", m.toolsLoading, m.toolsErr, cmd != nil)
	}
	next, cmd = m.Update(toolsListMsg{tools: []daemonclient.ToolInfo{{Name: "x"}}, skills: []daemonclient.Skill{{Name: "s"}}})
	m = next.(Model)
	if m.toolsErr != nil || len(m.toolsList) != 1 || m.toolsList[0].Name != "x" || len(m.skillsList) != 1 || cmd != nil {
		t.Fatalf("tools success = tools %+v skills %+v err=%v cmd=%v", m.toolsList, m.skillsList, m.toolsErr, cmd != nil)
	}

	m.wizardToolSettingsPending = true
	next, cmd = m.Update(toolSettingsUpdatedMsg{err: errors.New("sync")})
	m = next.(Model)
	if m.wizardToolSettingsPending || !strings.Contains(m.toolsStatus, "sync") || cmd != nil {
		t.Fatalf("settings error = pending %v status %q cmd=%v", m.wizardToolSettingsPending, m.toolsStatus, cmd != nil)
	}
	m.wizardToolSettingsPending = true
	next, cmd = m.Update(toolSettingsUpdatedMsg{})
	m = next.(Model)
	if m.wizardToolSettingsPending || m.phase != phaseWizardSystemCheck || m.wizardStep != 7 || cmd != nil {
		t.Fatalf("settings success = pending %v phase %d step %d cmd=%v", m.wizardToolSettingsPending, m.phase, m.wizardStep, cmd != nil)
	}
	next, cmd = m.Update(answerSentMsg{err: errors.New("answer")})
	m = next.(Model)
	if m.err == nil || m.err.Error() != "answer" || cmd != nil {
		t.Fatalf("answer error state = %v cmd=%v", m.err, cmd != nil)
	}

	initialFrame := m.animFrame
	next, cmd = m.Update(animTickMsg(time.Now()))
	m = next.(Model)
	if m.animFrame != initialFrame || cmd != nil {
		t.Fatal("idle animation tick should be ignored")
	}
	m.waiting = true
	next, cmd = m.Update(animTickMsg(time.Now()))
	m = next.(Model)
	if m.animFrame != initialFrame+1 || cmd == nil {
		t.Fatalf("active animation = frame %d cmd=%v", m.animFrame, cmd != nil)
	}
	m.pickerBusy = true
	next, cmd = m.Update(spinner.TickMsg{Time: time.Now()})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("active spinner tick did not schedule its next tick")
	}

	w := newWizardModel("", false)
	next, cmd = w.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	w = next.(Model)
	if !w.ready || w.width != 80 || w.height != 24 || cmd != nil {
		t.Fatalf("wizard window state = ready %v %dx%d cmd=%v", w.ready, w.width, w.height, cmd != nil)
	}
	for i := 0; i < splashTotalFrames; i++ {
		next, cmd = w.Update(splashTickMsg{})
		w = next.(Model)
		if i < splashTotalFrames-1 && cmd == nil {
			t.Fatalf("splash frame %d did not schedule next frame", i)
		}
	}
	if w.phase != phaseWizardEnvironment || w.splashFrame < splashTotalFrames {
		t.Fatalf("splash completion = phase %d frame %d", w.phase, w.splashFrame)
	}
}

func TestStreamEventsAndTranscriptBranches(t *testing.T) {
	m := testModel(t)
	m.messages = []chatMessage{{Role: "assistant", streaming: true}}
	events := []daemonclient.StreamEvent{
		{Type: "delta", Content: "partial"},
		{Type: "tool_start", Name: "bash", Args: "ls"},
		{Type: "tool_result", Name: "bash", Result: "ok", OK: true, ProcessID: "bg1"},
		{Type: "notice", Text: "notice"},
		{Type: "question", QuestionID: "q", Question: "why", Options: []string{"a"}},
		{Type: "approval", ApprovalID: "a", Tool: "bash", Subject: "ls", Options: []string{"once"}},
		{Type: "route_start"},
		{Type: "heartbeat"},
		{Type: "segment", Segment: 2, Segments: 4},
		{Type: "route_done", RouteCall: &daemonclient.RouteCall{Index: 1}},
	}
	for _, evt := range events {
		next, cmd := m.handleStreamEvent(streamEventMsg{event: evt})
		m = next.(Model)
		if cmd == nil {
			t.Fatalf("non-terminal stream event %q did not continue stream polling", evt.Type)
		}
	}
	if len(m.messages) != 1 || m.messages[0].Content != "partial" || len(m.messages[0].ToolActivity) != 1 ||
		m.messages[0].ToolActivity[0].Result != "ok" || len(m.messages[0].Notices) != 1 ||
		m.question == nil || m.question.id != "q" || m.approval == nil || m.approval.id != "a" ||
		m.routeCall == nil || m.routeCall.Index != 1 || m.routeRunning || m.heartbeats != 1 || m.segment != 2 || m.segments != 4 ||
		m.messages[0].ToolActivity[0].ProcessID != "bg1" {
		t.Fatalf("stream event aggregate state = messages %+v question %+v approval %+v route %+v running=%v", m.messages, m.question, m.approval, m.routeCall, m.routeRunning)
	}
	next, cmd := m.handleStreamEvent(streamEventMsg{err: errors.New("read")})
	m = next.(Model)
	if m.err == nil || m.err.Error() != "read" || m.waiting || cmd != nil {
		t.Fatalf("stream error state = err %v waiting %v cmd=%v", m.err, m.waiting, cmd != nil)
	}

	m.waitStartedAt = time.Now().Add(-10 * time.Second)
	m.lastEventAt = time.Now().Add(-10 * time.Second)
	if got := m.thinkingLine(); !strings.Contains(got, "CONEXÃO SEM EVENTOS") {
		t.Fatalf("stalled thinking line = %q", got)
	}
	m.lastEventAt = time.Now()
	m.workState = workModelActive
	if got := m.thinkingLine(); !strings.Contains(got, "MODELO ATIVO") || strings.Contains(got, "CONEXÃO SEM EVENTOS") {
		t.Fatalf("active thinking line = %q", got)
	}
}

func TestCommandClosuresAgainstLocalHTTPServer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/sessions" && r.Method == http.MethodGet:
			fmt.Fprint(w, `[{"id":"s","title":"one"}]`)
		case r.URL.Path == "/sessions" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"id":"new","title":"made"}`)
		case r.URL.Path == "/sessions/s":
			fmt.Fprint(w, `{"session":{"id":"s"},"messages":[{"role":"user","content":"hi"}]}`)
		case r.URL.Path == "/sessions/s/context":
			fmt.Fprint(w, `{"budget":100,"used":10}`)
		case r.URL.Path == "/admin/status":
			fmt.Fprint(w, `{"combos":[{"id":"c","strategy":"priority","providers":["p"]}]}`)
		case r.URL.Path == "/sessions/s/messages":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintln(w, `data: {"type":"delta","content":"hi"}`)
			fmt.Fprintln(w, `data: [DONE]`)
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	defer server.Close()
	dc := daemonclient.New(server.URL)
	sc := statusclient.New(server.URL)

	for name, cmd := range map[string]tea.Cmd{
		"list": listSessionsCmd(dc), "create": createSessionCmd(dc, "made"), "history": loadHistoryCmd(dc, "s"),
		"send": startSendMessageCmd(dc, "s", "hi"), "status": fetchStatusCmd(sc), "context": fetchContextCmd(dc, "s"),
		"tools": fetchToolsCmd(dc), "question": answerQuestionCmd(dc, "s", "q", "a"), "approval": answerApprovalCmd(dc, "s", "a", "once"),
	} {
		if msg := cmd(); msg == nil {
			t.Errorf("%s command returned nil", name)
		}
	}
	start := startSendMessageCmd(dc, "s", "hi")().(streamStartMsg)
	if start.err != nil {
		t.Fatal(start.err)
	}
	if msg := readNextEventCmd(start.stream)().(streamEventMsg); msg.event.Content != "hi" {
		t.Fatalf("stream event = %+v", msg)
	}
	if err := start.stream.Close(); err != nil {
		t.Fatalf("close stream: %v", err)
	}

	for _, terminal := range []daemonclient.StreamEvent{
		{Type: "done", Message: daemonclient.Message{Content: "final", Provider: "p"}, Usage: openai.Usage{TotalTokens: 9}},
		{Type: "error", Error: "failed"},
	} {
		stream := startSendMessageCmd(dc, "s", "hi")().(streamStartMsg).stream
		m := testModel(t)
		m.waiting = true
		m.messages = []chatMessage{{Role: "assistant", streaming: true}}
		next, cmd := m.handleStreamEvent(streamEventMsg{stream: stream, event: terminal})
		if next.(Model).waiting {
			t.Fatalf("terminal stream event %q left model waiting", terminal.Type)
		}
		if terminal.Type == "done" && cmd == nil {
			t.Fatal("successful terminal event did not refresh context")
		}
		if terminal.Type == "error" && cmd != nil {
			t.Fatal("failed terminal event unexpectedly scheduled follow-up")
		}
	}
	stream := startSendMessageCmd(dc, "s", "hi")().(streamStartMsg).stream
	mEOF := testModel(t)
	mEOF.waiting = true
	mEOF.messages = []chatMessage{{Role: "assistant", streaming: true, Content: "partial"}}
	nextEOF, eofCmd := mEOF.handleStreamEvent(streamEventMsg{stream: stream, done: true})
	if nextEOF.(Model).messages[0].streaming || nextEOF.(Model).waiting || eofCmd != nil {
		t.Fatal("EOF did not settle placeholder")
	}

	settingsDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", settingsDir)
	settings, err := toolsettings.Load()
	if err != nil {
		t.Fatal(err)
	}
	if msg := syncToolSettingsCmd(nil, settings)().(toolSettingsUpdatedMsg); msg.err == nil {
		t.Fatal("nil daemon should fail")
	}
	if msg := syncToolSettingsCmd(dc, settings)().(toolSettingsUpdatedMsg); msg.err != nil {
		t.Fatalf("sync tool settings: %v", msg.err)
	}

	if got := startOAuthCmd("unknown")().(oauthURLMsg); got.err == nil {
		t.Fatal("unknown oauth should fail")
	}
	perm := waitOAuthPermanentCmd(context.Background(), "x", func(context.Context) (string, error) { return "key", nil })().(oauthResultMsg)
	if perm.key != "key" {
		t.Fatal("permanent oauth wait lost key")
	}
	refresh := waitOAuthRefreshableCmd(context.Background(), "x", func(context.Context) (oauthflow.Token, error) { return oauthflow.Token{Access: "a"}, nil })().(oauthResultMsg)
	if !refresh.refreshable {
		t.Fatal("refresh oauth wait not marked")
	}
}

func TestPingAccountsCommandCoversCatalogAndCustomProviders(t *testing.T) {
	clearAllCatalogProviderEnvVars(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cred, err := credentials.Load()
	if err != nil {
		t.Fatal(err)
	}
	custom, err := customprovider.Load()
	if err != nil {
		t.Fatal(err)
	}
	cp, err := custom.Add("local", server.URL, "model", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := cred.Set(cp.EnvVar, "custom-key"); err != nil {
		t.Fatalf("store custom key: %v", err)
	}
	// One configured catalog row covers resolution, adapter lookup and its
	// concurrent worker without contacting a public provider: override the
	// catalog key only when its production base URL is local isn't possible,
	// so custom exercises the HTTP worker and the catalog remains unchecked.
	msg := pingAccountsCmd(cred, []customprovider.Provider{cp})().(pingResultsMsg)
	if got := msg.results[cp.EnvVar].Status; got != providerping.StatusOK {
		t.Fatalf("custom ping status = %v", got)
	}
	if got := pingAccountsCmd(nil, nil)().(pingResultsMsg); len(got.results) != 0 {
		t.Fatalf("empty credentials unexpectedly pinged: %v", got.results)
	}
}

func TestInitPhaseDispatchMouseAndSplashKeys(t *testing.T) {
	m := testModel(t)
	for _, p := range []phase{phasePicker, phaseWizardToolsPreset, phaseWizardEnvironment, phaseChat} {
		m.phase = p
		if initCmd, phaseCmd := m.Init(), m.phaseInitCmd(p); initCmd == nil || phaseCmd == nil {
			t.Fatalf("phase %d should initialize commands: Init=%v phaseInit=%v", p, initCmd != nil, phaseCmd != nil)
		}
	}
	m.phase = phaseAccounts
	if initCmd, phaseCmd := m.Init(), m.phaseInitCmd(phaseAccounts); initCmd != nil || phaseCmd != nil {
		t.Fatalf("accounts phase should be idle: Init=%v phaseInit=%v", initCmd != nil, phaseCmd != nil)
	}
	m.phase = phaseSplash
	if cmd := m.Init(); cmd == nil {
		t.Fatal("splash Init did not schedule animation")
	}
	for _, key := range []string{"x", "enter"} {
		m.phase = phaseSplash
		m.splashTarget = phasePicker
		beforeFrame := m.splashFrame
		var cmd tea.Cmd
		m, cmd = modelResult(m.handleKey(keyMsg(key)))
		if key == "x" {
			if m.phase != phaseSplash || m.splashFrame != beforeFrame || cmd != nil {
				t.Fatal("unrecognized splash key should leave splash untouched")
			}
		} else if m.phase != phasePicker || cmd == nil {
			t.Fatalf("enter did not finish splash: phase %d cmd=%v", m.phase, cmd != nil)
		}
	}

	m = testModel(t)
	m.viewport.SetContent(strings.Repeat("line\n", 100))
	m.viewport.GotoBottom()
	bottom := m.viewport.YOffset
	m, cmd := modelResult(m.handleMouse(tea.MouseMsg{Button: tea.MouseButtonWheelUp}))
	if m.viewport.YOffset >= bottom || cmd != nil {
		t.Fatalf("wheel up = offset %d from %d cmd=%v", m.viewport.YOffset, bottom, cmd != nil)
	}
	up := m.viewport.YOffset
	m, cmd = modelResult(m.handleMouse(tea.MouseMsg{Button: tea.MouseButtonWheelDown}))
	if m.viewport.YOffset <= up || cmd != nil {
		t.Fatalf("wheel down = offset %d from %d cmd=%v", m.viewport.YOffset, up, cmd != nil)
	}
	for _, mouse := range []tea.MouseMsg{
		{Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft},
		{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 0, Y: 0},
	} {
		before := m.active
		m, cmd = modelResult(m.handleMouse(mouse))
		if m.active != before || cmd != nil {
			t.Fatalf("irrelevant mouse event mutated panel from %d to %d", before, m.active)
		}
	}
	m, cmd = modelResult(m.handleMouse(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: m.width - 1, Y: m.height - 1}))
	if m.active != panelContext || cmd == nil {
		t.Fatalf("footer context click = panel %d cmd=%v", m.active, cmd != nil)
	}
	m.phase = phaseSplash
	beforeOffset := m.viewport.YOffset
	m, cmd = modelResult(m.handleMouse(tea.MouseMsg{Button: tea.MouseButtonWheelUp}))
	if m.viewport.YOffset != beforeOffset || cmd != nil {
		t.Fatal("splash should ignore mouse input")
	}
}

func TestUpdateOAuthSuccessAndRoutingBranches(t *testing.T) {
	m := testModel(t)
	t.Setenv("PATH", t.TempDir())
	m.accountsOAuthPending = true
	next, cmd := m.Update(oauthURLMsg{
		acctID: "anthropic", url: "http://auth",
		waitPermanent: func(context.Context) (string, error) { return "key", nil },
	})
	m = next.(Model)
	if cmd == nil || m.accountsOAuthCancel == nil {
		t.Fatal("OAuth URL did not start waiter")
	}
	next, follow := m.Update(cmd())
	m = next.(Model)
	if m.accountsStatus == "" || follow != nil {
		t.Fatal("OAuth result did not set status")
	}

	m.wizardMode = true
	m.accountsOAuthPending = true
	next, cmd = m.Update(oauthURLMsg{
		acctID: "openai-chatgpt", url: "http://auth",
		waitRefreshable: func(context.Context) (oauthflow.Token, error) { return oauthflow.Token{Access: "a"}, nil },
	})
	m = next.(Model)
	next, follow = m.Update(cmd())
	if !next.(Model).accountsPinging || follow == nil {
		t.Fatal("wizard OAuth result should trigger verification")
	}

	m = testModel(t)
	m.wizardToolSettingsPending = true
	next, follow = m.Update(toolSettingsUpdatedMsg{})
	if next.(Model).phase != phaseWizardSystemCheck || follow != nil {
		t.Fatal("tool settings acknowledgement did not advance wizard")
	}

	for _, p := range []phase{phaseAccounts, phaseTools, phaseWizardEnvironment, phaseWizardProjects, phaseWizardRouting, phaseWizardPermissions, phaseWizardToolsPreset, phaseWizardSystemCheck, phaseWizardSummary} {
		m.phase = p
		m, follow = modelResult(m.handleKey(keyMsg("x")))
		if m.phase != p || follow != nil {
			t.Fatalf("irrelevant key mutated phase %d to %d or returned command", p, m.phase)
		}
	}
}

func TestAccountsRenderingAndKeyBoundaryMatrix(t *testing.T) {
	clearAllCatalogProviderEnvVars(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cred, err := credentials.Load()
	if err != nil {
		t.Fatal(err)
	}
	custom, err := customprovider.Load()
	if err != nil {
		t.Fatal(err)
	}
	cp, err := custom.Add("Local", "http://localhost/v1", "m", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := cred.Set(cp.EnvVar, "key"); err != nil {
		t.Fatal(err)
	}
	if err := cred.Set(providercatalog.Accounts[0].EnvVar, "stored"); err != nil {
		t.Fatal(err)
	}
	t.Setenv(providercatalog.Accounts[1].EnvVar, "environment")
	m := testModel(t)
	m.credStore, m.customStore, m.customProviders = cred, custom, custom.All()
	m.accountsPings = map[string]providerping.Result{
		providercatalog.Accounts[0].EnvVar: {Status: providerping.StatusOK, Latency: 10 * time.Millisecond},
		cp.EnvVar:                          {Status: providerping.StatusDegraded, Latency: 2 * time.Second},
	}
	for cursor := 0; cursor < len(providercatalog.Accounts)+len(m.customProviders)+1; cursor++ {
		m.accountsCursor = cursor
		if got := m.renderAccounts(); !strings.Contains(got, "Local") || !strings.Contains(got, "+ adicionar") {
			t.Fatalf("accounts cursor %d omitted registered rows: %q", cursor, got)
		}
	}
	m.wizardMode, m.wizardWorkspaceLocked = true, false
	if got := m.renderAccounts(); !strings.Contains(got, "esc volta") {
		t.Fatalf("unlocked wizard hints = %q", got)
	}
	m.accountsPings = map[string]providerping.Result{
		providercatalog.Accounts[0].EnvVar: {Status: providerping.StatusDown},
		cp.EnvVar:                          {Status: providerping.StatusDown},
	}
	m.wizardProviderOverrideVisible = true
	if got := m.renderAccounts(); !strings.Contains(got, "c continua") {
		t.Fatalf("override hints = %q", got)
	}
	m.wizardWorkspaceLocked = true
	if got := m.renderAccounts(); !strings.Contains(got, "esc cancela") {
		t.Fatalf("locked wizard hints = %q", got)
	}

	m.accountsCursor = len(providercatalog.Accounts)
	if _, _, ok := m.currentCredentialTarget(); !ok {
		t.Fatal("custom credential target missing")
	}
	m.accountsCursor = len(providercatalog.Accounts) + len(m.customProviders)
	if _, _, ok := m.currentCredentialTarget(); ok {
		t.Fatal("add row unexpectedly has credential target")
	}

	m.wizardMode = false
	m.accountsCursor = len(providercatalog.Accounts)
	m, cmd := modelResult(m.handleAccountsKey(keyMsg("enter")))
	if !m.accountsEditing || cmd == nil {
		t.Fatal("custom credential editor did not open")
	}
	m.accountsKeyInput.SetValue("")
	m, cmd = modelResult(m.handleAccountsKey(keyMsg("enter")))
	if m.accountsEditing || cmd != nil {
		t.Fatal("empty credential should close without command")
	}
	m.accountsOAuthPending = true
	ctx, cancel := context.WithCancel(context.Background())
	m.accountsOAuthCancel = cancel
	m, cmd = modelResult(m.handleAccountsKey(keyMsg("esc")))
	if m.accountsOAuthPending || m.accountsOAuthCancel != nil || cmd != nil {
		t.Fatal("OAuth escape did not clear pending state")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("OAuth cancel was not invoked")
	}

	m.wizardMode, m.wizardWorkspaceLocked = true, true
	m, cmd = modelResult(m.handleAccountsKey(keyMsg("esc")))
	if !m.wizardCancelled || cmd == nil {
		t.Fatal("locked wizard escape did not cancel")
	}
	m.wizardWorkspaceLocked = false
	m.wizardCancelled = false
	m, cmd = modelResult(m.handleAccountsKey(keyMsg("esc")))
	if m.phase != phaseWizardProjects || m.wizardCancelled || cmd != nil {
		t.Fatal("unlocked wizard escape did not return to projects")
	}

	clearAllCatalogProviderEnvVars(t)
	m = testModel(t)
	m.wizardMode = true
	m, cmd = modelResult(m.handleAccountsKey(keyMsg("n")))
	if m.accountsStatus == "" || m.phase == phaseWizardRouting || cmd != nil {
		t.Fatal("empty provider wizard should explain block")
	}
}

func TestCredentialAndCustomProviderSuccessPaths(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cred, err := credentials.Load()
	if err != nil {
		t.Fatal(err)
	}
	custom, err := customprovider.Load()
	if err != nil {
		t.Fatal(err)
	}
	m := testModel(t)
	m.credStore, m.customStore = cred, custom
	m.accountsCursor, m.accountsEditing = 0, true
	m.accountsKeyInput.SetValue(" secret ")
	m, cmd := modelResult(m.handleAccountsKey(keyMsg("enter")))
	if cred.Get(m.accountRowsEnv(0)) == "" || m.accountsEditing || cmd != nil {
		t.Fatal("catalog key not saved")
	}

	m.accountsAddingCustom = true
	m.customFormInputs = newCustomProviderFormInputs()
	for i, value := range []string{"Lab", "http://127.0.0.1:1/v1", "key", "model", "n"} {
		m.customFormInputs[i].SetValue(value)
	}
	m, cmd = modelResult(m.handleCustomProviderFormKey(keyMsg("enter")))
	if len(m.customProviders) != 1 || m.customProviders[0].SupportsToolsOrDefault() || m.accountsAddingCustom || cmd != nil {
		t.Fatalf("custom provider = %+v", m.customProviders)
	}

	status, err := saveOAuthResult(cred, oauthResultMsg{acctID: "anthropic", key: "oauth-key"})
	if err != nil || status == "" {
		t.Fatalf("save OAuth key: %q %v", status, err)
	}
	refreshStatus, err := saveOAuthResult(cred, oauthResultMsg{acctID: "openai-chatgpt", refreshable: true, token: oauthflow.Token{Access: "a", Refresh: "r", ExpiresAt: time.Now().Add(time.Hour)}})
	if err != nil || refreshStatus == "" {
		t.Fatalf("save refreshable OAuth result: status=%q err=%v", refreshStatus, err)
	}
	if _, err := saveOAuthResult(cred, oauthResultMsg{acctID: "missing"}); err == nil {
		t.Fatal("unknown OAuth account should fail")
	}
}

func (m Model) accountRowsEnv(i int) string {
	// Tests stay independent of catalog ordering details beyond the same row
	// the production editor selected.
	env, _, _ := m.currentCredentialTarget()
	return env
}

func TestMiscellaneousErrorAndBoundaryBranches(t *testing.T) {
	m := testModel(t)
	m.width, m.height = 0, 3
	m.syncViewportSize()
	if m.viewport.Height != 3 {
		t.Fatalf("viewport height = %d", m.viewport.Height)
	}
	m.height = 60
	if m.panelHeight() != 20 {
		t.Fatalf("panel height = %d", m.panelHeight())
	}

	icons := make(map[string]bool)
	for _, used := range []int{0, 75, 95} {
		m.haveContext = true
		m.contextData = daemonclient.ContextUsage{Used: used, Budget: 100}
		icons[m.contextIcon()] = true
	}
	if len(icons) != 3 {
		t.Fatalf("context thresholds produced only %d distinct icons", len(icons))
	}
	m.haveContext = false
	if got := m.contextIcon(); got == "" || icons[got] {
		t.Fatalf("unknown context icon = %q", got)
	}

	for _, tc := range []struct {
		act  daemonclient.ToolActivity
		want string
	}{
		{daemonclient.ToolActivity{Name: "run", Running: true}, m.spin.View()},
		{daemonclient.ToolActivity{Name: "ok", OK: true}, "✓"},
		{daemonclient.ToolActivity{Name: "bad", OK: false}, "✗"},
	} {
		if got := m.renderToolActivity(tc.act); !strings.Contains(got, tc.act.Name) || !strings.Contains(got, tc.want) {
			t.Fatalf("tool activity %+v = %q", tc.act, got)
		}
	}
	categoryColors := map[string]bool{}
	for _, category := range []string{"messages", "tool_overview", "project_context", "memory", "compaction_summary", "other"} {
		categoryColors[fmt.Sprint(categoryStyle(category).GetForeground())] = true
	}
	if len(categoryColors) < 5 {
		t.Fatalf("category palette collapsed to %d colors", len(categoryColors))
	}

	inputs := newCustomProviderFormInputs()
	if len(inputs) != len(customFormLabels) {
		t.Fatal("form input mismatch")
	}
	if parseSupportsToolsInput("não") || !parseSupportsToolsInput("yes") {
		t.Fatal("tools parsing")
	}
	if oauthRefreshAdapter("missing") != nil {
		t.Fatal("unknown refresh adapter should be nil")
	}
	if cmd := browserCommand("http://example.com"); cmd == nil || !strings.Contains(strings.Join(cmd.Args, " "), "http://example.com") {
		t.Fatalf("browser command did not retain URL: %#v", cmd)
	}

	bad := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(bad, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := newWizardModel("", false)
	w.wizardProjectsRootInput.SetValue(t.TempDir())
	w.wizardWorkspaceInput.SetValue(filepath.Join(bad, "child"))
	w, cmd := modelResult(w.handleWizardProjectsKey(keyMsg("enter")))
	if w.wizardWorkspaceErr == nil || cmd != nil || w.phase == phaseAccounts {
		t.Fatal("expected invalid workspace error")
	}
}

func TestCoverageMarginAcrossWizardAndPresentationBranches(t *testing.T) {
	w := newWizardModel("", false)
	w.ready, w.width, w.height = true, 90, 30
	w, cmd := modelResult(w.handleWizardEnvironmentKey(keyMsg("esc")))
	if !w.wizardCancelled || cmd == nil {
		t.Fatal("environment escape must cancel")
	}
	w = newWizardModel(t.TempDir(), true)
	w, cmd = modelResult(w.handleWizardEnvironmentKey(keyMsg("enter")))
	if w.phase != phaseAccounts || cmd == nil {
		t.Fatal("locked workspace should skip projects")
	}

	w = newWizardModel("", false)
	w.phase = phaseWizardProjects
	w, cmd = modelResult(w.handleWizardProjectsKey(keyMsg("esc")))
	if w.phase != phaseWizardEnvironment || w.wizardStep != 1 || cmd != nil {
		t.Fatal("projects escape did not go back")
	}
	w.phase = phaseWizardProjects
	w.wizardProjectsRootInput.SetValue(t.TempDir())
	w.wizardWorkspaceInput.SetValue("")
	w, cmd = modelResult(w.handleWizardProjectsKey(keyMsg("enter")))
	if w.wizardChosenWorkspace == "" || w.wizardChosenWorkspace != w.wizardProjectsRoot || w.phase != phaseAccounts || cmd == nil {
		t.Fatal("empty workspace did not default to projects root")
	}
	w = newWizardModel("", false)
	w.wizardProjectsField = 1
	w.wizardProjectsRootInput.Blur()
	w.wizardWorkspaceInput.Focus()
	beforeWorkspace := w.wizardWorkspaceInput.Value()
	w, cmd = modelResult(w.handleWizardProjectsKey(keyMsg("x")))
	if w.wizardWorkspaceInput.Value() == beforeWorkspace {
		t.Fatal("workspace input did not receive key")
	}
	w.wizardPermCursor = 0
	w, cmd = modelResult(w.handleWizardPermissionsKey(keyMsg("up")))
	if w.wizardPermCursor != 0 || cmd != nil {
		t.Fatal("permissions cursor moved above first row")
	}
	w.wizardPermCursor = len(wizardPermOptions) - 1
	w, cmd = modelResult(w.handleWizardPermissionsKey(keyMsg("down")))
	if w.wizardPermCursor != len(wizardPermOptions)-1 || cmd != nil {
		t.Fatal("permissions cursor moved below last row")
	}
	w, cmd = modelResult(w.handleWizardPermissionsKey(keyMsg("esc")))
	if w.phase != phaseWizardRouting || w.wizardStep != 4 || cmd != nil {
		t.Fatal("permissions escape did not return to routing")
	}

	m := testModel(t)
	m.toolsLoading = true
	if got := m.renderWizardToolsPreset(); !strings.Contains(got, "carregando") {
		t.Fatalf("loading preset view = %q", got)
	}
	beforePreset := m.wizardChosenToolsPreset
	m, cmd = modelResult(m.handleWizardToolsPresetKey(keyMsg("enter")))
	if m.wizardChosenToolsPreset != beforePreset || cmd != nil {
		t.Fatal("preset input should be blocked while loading")
	}
	m.toolsLoading = false
	m.toolsStatus = "applied"
	if got := m.renderWizardToolsPreset(); !strings.Contains(got, "applied") {
		t.Fatalf("preset status view = %q", got)
	}
	m.wizardWelcomeSession = true
	m.messages = nil
	m.refreshTranscript()
	if !strings.Contains(m.viewport.View(), "Kram está pronto") {
		t.Fatal("wizard welcome was not rendered")
	}
	if got := m.renderWizardWelcomeBanner(); !strings.Contains(got, "Kram está pronto") || !strings.Contains(got, "Map this repository") {
		t.Fatalf("welcome banner = %q", got)
	}

	if got := gradientText("gradient", bannerRGB{1, 2, 3}, bannerRGB{4, 5, 6}, 0); !strings.Contains(got, "gradient") {
		t.Fatalf("gradient output = %q", got)
	}
	if got := renderMarkdown(nil, "plain"); got != "plain" {
		t.Fatalf("nil markdown renderer = %q", got)
	}
	r := newMarkdownRenderer(40)
	if got := renderMarkdown(r, "**bold**"); !strings.Contains(got, "bold") {
		t.Fatalf("markdown output = %q", got)
	}
	previousReveal, previousFade := -1.0, -1.0
	for frame := 0; frame < splashTotalFrames; frame++ {
		reveal, fade := splashAnimation(frame)
		if reveal < 0 || reveal > 1 || fade < 0 || fade > 1 || reveal < previousReveal || (fade > 0 && fade < previousFade) {
			t.Fatalf("invalid splash animation frame %d: reveal=%v fade=%v", frame, reveal, fade)
		}
		previousReveal, previousFade = reveal, fade
	}

	combo := &statusclient.Combo{Providers: []string{"a", "b"}}
	if got := explainProvider(combo, map[string]statusclient.Provider{}, -1); got != "" {
		t.Fatalf("invalid focus explanation = %q", got)
	}
	if got := explainProvider(combo, map[string]statusclient.Provider{}, 0); !strings.Contains(got, "sem requisições") {
		t.Fatalf("unused provider explanation = %q", got)
	}
	if got := explainProvider(combo, map[string]statusclient.Provider{"a": {ID: "a", BreakerOpen: true}}, 0); !strings.Contains(got, "circuito aberto") {
		t.Fatalf("open breaker explanation = %q", got)
	}
	if got := explainProvider(combo, map[string]statusclient.Provider{"b": {ID: "b", Stats: statusclient.ProviderStats{Requests: 2, SuccessRate: .5, AvgLatencyMS: 20}}}, 1); !strings.Contains(got, "2 requisições") || !strings.Contains(got, "50%") || !strings.Contains(got, "20ms") {
		t.Fatalf("provider stats explanation = %q", got)
	}

	m.messages = nil
	m.dropStreamingPlaceholder()
	if len(m.messages) != 0 {
		t.Fatal("empty transcript changed while dropping placeholder")
	}
	m.messages = []chatMessage{{Role: "assistant", Content: "done"}}
	m.dropStreamingPlaceholder()
	if len(m.messages) != 1 || m.messages[0].Content != "done" {
		t.Fatal("settled message was incorrectly dropped")
	}
	m.customStore = nil
	m.customProviders = []customprovider.Provider{{ID: "stale"}}
	m.refreshCustomProviders()
	if len(m.customProviders) != 0 {
		t.Fatal("nil custom store did not clear cached providers")
	}
}

func TestAccountsAdditionalKeyPaths(t *testing.T) {
	clearAllCatalogProviderEnvVars(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cred, err := credentials.Load()
	if err != nil {
		t.Fatal(err)
	}
	m := testModel(t)
	m.credStore = cred
	m.accountsEditing = true
	m.accountsKeyInput.SetValue("x")
	m.accountsCursor = len(providercatalog.Accounts) // add row: invalid credential target
	m, cmd := modelResult(m.handleAccountsKey(keyMsg("enter")))
	if m.accountsEditing || cred.Get(providercatalog.Accounts[0].EnvVar) != "" || cmd != nil {
		t.Fatal("invalid add-row credential target should close without saving")
	}

	m.accountsEditing = true
	m, cmd = modelResult(m.handleAccountsKey(keyMsg("esc")))
	if m.accountsEditing || cmd != nil {
		t.Fatal("credential escape did not close editor")
	}
	m.accountsEditing = true
	m.accountsKeyInput.SetValue("")
	m.accountsKeyInput.Focus()
	m, cmd = modelResult(m.handleAccountsKey(keyMsg("x")))
	if m.accountsKeyInput.Value() != "x" {
		t.Fatalf("credential input = %q", m.accountsKeyInput.Value())
	}

	m.accountsEditing = false
	m.accountsCursor = 0
	m, cmd = modelResult(m.handleAccountsKey(keyMsg("o")))
	if !m.accountsOAuthPending || cmd == nil {
		t.Fatal("supported OAuth account did not start authorization")
	}
	m.accountsOAuthPending = false
	m.accountsCursor = len(providercatalog.Accounts)
	m, cmd = modelResult(m.handleAccountsKey(keyMsg("o")))
	if m.accountsOAuthPending || cmd != nil {
		t.Fatal("custom add row unexpectedly started OAuth")
	}
	phaseBefore := m.phase
	m, cmd = modelResult(m.handleAccountsKey(keyMsg("c")))
	if m.phase != phaseBefore || cmd != nil {
		t.Fatal("non-wizard override key mutated accounts")
	}

	m.accountsAddingCustom = true
	m.customFormInputs = newCustomProviderFormInputs()
	m.customStore = nil
	m, cmd = modelResult(m.handleCustomProviderFormKey(keyMsg("enter")))
	if m.accountsStatus == "" || !m.accountsAddingCustom || cmd != nil {
		t.Fatal("missing custom store should be reported")
	}

	m.accountsAddingCustom = true
	m.customStore, err = customprovider.Load()
	if err != nil {
		t.Fatal(err)
	}
	m.customFormInputs = newCustomProviderFormInputs()
	m.customFormInputs[0].SetValue("")
	m.customFormInputs[1].SetValue("bad")
	m, cmd = modelResult(m.handleCustomProviderFormKey(keyMsg("enter")))
	if m.accountsStatus == "" || !m.accountsAddingCustom || cmd != nil {
		t.Fatal("invalid custom provider should be reported")
	}
}
