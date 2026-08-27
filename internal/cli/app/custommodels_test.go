package app

import (
	"errors"
	"strings"
	"testing"
)

func newTestCustomForm(t *testing.T) Model {
	t.Helper()
	m := testModel(t)
	m.accountsAddingCustom = true
	m.customFormInputs = newCustomProviderFormInputs()
	return m
}

func TestCtrlLOnlyTriggersFetchOnModelField(t *testing.T) {
	m := newTestCustomForm(t)
	m.customFormInputs[1].SetValue("http://192.168.0.4:1234/v1")
	m.customFormCursor = 0 // name field, not model

	next, cmd := m.handleCustomProviderFormKey(keyMsg("ctrl+l"))
	m = next.(Model)
	if cmd != nil || m.customFormFetchingModels {
		t.Fatalf("ctrl+l on a non-model field should be a no-op, got fetching=%v cmd=%v", m.customFormFetchingModels, cmd != nil)
	}
}

func TestCtrlLRequiresURLFirst(t *testing.T) {
	m := newTestCustomForm(t)
	m.customFormCursor = 3 // model field
	// url (index 1) left empty

	next, cmd := m.handleCustomProviderFormKey(keyMsg("ctrl+l"))
	m = next.(Model)
	if cmd != nil || m.customFormFetchingModels || !strings.Contains(m.accountsStatus, "url") {
		t.Fatalf("ctrl+l with no url should ask for one first, got status=%q fetching=%v cmd=%v", m.accountsStatus, m.customFormFetchingModels, cmd != nil)
	}
}

func TestCtrlLStartsFetchWhenURLPresent(t *testing.T) {
	m := newTestCustomForm(t)
	m.customFormInputs[1].SetValue("http://192.168.0.4:1234/v1")
	m.customFormInputs[2].SetValue("sk-test")
	m.customFormCursor = 3

	next, cmd := m.handleCustomProviderFormKey(keyMsg("ctrl+l"))
	m = next.(Model)
	if cmd == nil || !m.customFormFetchingModels {
		t.Fatalf("ctrl+l with a url present should start a fetch, got fetching=%v cmd=%v", m.customFormFetchingModels, cmd != nil)
	}
}

func TestCustomModelListMsgErrorFallsBackToManualTyping(t *testing.T) {
	m := newTestCustomForm(t)
	m.customFormFetchingModels = true

	next, cmd := m.Update(customModelListMsg{err: errors.New("unreachable")})
	m = next.(Model)
	if cmd != nil || m.customFormFetchingModels || m.customFormPickingModel || m.accountsStatus == "" {
		t.Fatalf("fetch error should clear fetching, not enter picking mode, and leave a status message: %+v", m)
	}
}

func TestCustomModelListMsgEmptyFallsBackToManualTyping(t *testing.T) {
	m := newTestCustomForm(t)
	m.customFormFetchingModels = true

	next, _ := m.Update(customModelListMsg{models: nil})
	m = next.(Model)
	if m.customFormPickingModel || m.accountsStatus == "" {
		t.Fatalf("an empty model list should not enter picking mode, and should explain why: %+v", m)
	}
}

func TestCustomModelListMsgSuccessEntersPickingMode(t *testing.T) {
	m := newTestCustomForm(t)
	m.customFormFetchingModels = true

	next, _ := m.Update(customModelListMsg{models: []string{"a-model", "b-model"}})
	m = next.(Model)
	if !m.customFormPickingModel || m.customFormFetchingModels || len(m.customFormModelOptions) != 2 || m.customFormModelCursor != 0 {
		t.Fatalf("a successful fetch should enter picking mode with the models loaded: %+v", m)
	}
}

func TestPickerEnterFillsModelFieldAndExitsPicking(t *testing.T) {
	m := newTestCustomForm(t)
	m.customFormPickingModel = true
	m.customFormModelOptions = []string{"prism-ml/bonsai-27b", "qwen3.5-9b"}
	m.customFormModelCursor = 0

	next, cmd := m.handleCustomProviderFormKey(keyMsg("enter"))
	m = next.(Model)
	if cmd != nil || m.customFormPickingModel || m.customFormInputs[3].Value() != "prism-ml/bonsai-27b" {
		t.Fatalf("picker enter should fill field 3 and exit picking mode, got model=%q picking=%v", m.customFormInputs[3].Value(), m.customFormPickingModel)
	}
}

func TestPickerEscCancelsWithoutTouchingModelField(t *testing.T) {
	m := newTestCustomForm(t)
	m.customFormInputs[3].SetValue("original-typed-value")
	m.customFormPickingModel = true
	m.customFormModelOptions = []string{"other-model"}

	next, cmd := m.handleCustomProviderFormKey(keyMsg("esc"))
	m = next.(Model)
	if cmd != nil || m.customFormPickingModel || m.customFormInputs[3].Value() != "original-typed-value" {
		t.Fatalf("picker esc should cancel without changing the model field, got model=%q picking=%v", m.customFormInputs[3].Value(), m.customFormPickingModel)
	}
}

func TestPickerUpDownStayWithinBounds(t *testing.T) {
	m := newTestCustomForm(t)
	m.customFormPickingModel = true
	m.customFormModelOptions = []string{"one", "two", "three"}
	m.customFormModelCursor = 0

	next, _ := m.handleCustomProviderFormKey(keyMsg("up")) // already at 0
	m = next.(Model)
	if m.customFormModelCursor != 0 {
		t.Fatalf("cursor should not go below 0, got %d", m.customFormModelCursor)
	}

	for i := 0; i < 5; i++ {
		next, _ = m.handleCustomProviderFormKey(keyMsg("down"))
		m = next.(Model)
	}
	if m.customFormModelCursor != 2 {
		t.Fatalf("cursor should stop at the last index (2), got %d", m.customFormModelCursor)
	}
}

func TestVisibleModelIndicesWindowsAroundCursor(t *testing.T) {
	got := visibleModelIndices(50, 200)
	if len(got) != customFormModelListMaxRows {
		t.Fatalf("visibleModelIndices returned %d rows, want %d", len(got), customFormModelListMaxRows)
	}
	if got[0] > 50 || got[len(got)-1] < 50 {
		t.Fatalf("window %v does not contain the cursor (50)", got)
	}
}

func TestVisibleModelIndicesEmptyList(t *testing.T) {
	if got := visibleModelIndices(0, 0); got != nil {
		t.Fatalf("visibleModelIndices with 0 models = %v, want nil", got)
	}
}

func TestRenderCustomProviderFormShowsFetchingStatus(t *testing.T) {
	m := newTestCustomForm(t)
	m.customFormFetchingModels = true
	if got := m.renderCustomProviderForm(); !strings.Contains(got, "fetching models") {
		t.Errorf("form render during a fetch should show a loading indicator, got: %q", got)
	}
}

func TestRenderCustomProviderFormShowsPickerList(t *testing.T) {
	m := newTestCustomForm(t)
	m.customFormPickingModel = true
	m.customFormModelOptions = []string{"model-a", "model-b"}
	m.customFormModelCursor = 1

	got := m.renderCustomProviderForm()
	if !strings.Contains(got, "model-a") || !strings.Contains(got, "model-b") {
		t.Errorf("picker render should list every fetched model, got: %q", got)
	}
	if !strings.Contains(got, "enter use") {
		t.Errorf("picker render should show its own keybinding hint, got: %q", got)
	}
}

func TestRenderCustomProviderFormHintMentionsCtrlLOnlyOnModelField(t *testing.T) {
	m := newTestCustomForm(t)
	m.customFormCursor = 0
	if got := m.renderCustomProviderForm(); strings.Contains(got, "ctrl+l") {
		t.Errorf("hint should not mention ctrl+l off the model field, got: %q", got)
	}
	m.customFormCursor = 3
	if got := m.renderCustomProviderForm(); !strings.Contains(got, "ctrl+l") {
		t.Errorf("hint should mention ctrl+l on the model field, got: %q", got)
	}
}
