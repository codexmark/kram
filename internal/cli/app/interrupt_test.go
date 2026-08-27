package app

import (
	"errors"
	"strings"
	"testing"

	"github.com/codexmark/kram/internal/cli/daemonclient"
)

// TestEscInterruptsInFlightTurn confirms Esc, with a turn in flight and no
// panel open, closes the stream, stops waiting, and marks the tail message
// interrupted rather than doing nothing.
func TestEscInterruptsInFlightTurn(t *testing.T) {
	m := testModel(t)
	m.messages = []chatMessage{{Role: "assistant", streaming: true, Content: "partial"}}
	m.waiting = true
	m.activeStream = &interruptFakeStream // non-nil so the interrupt path fires

	next, _ := m.handleKey(keyMsg("esc"))
	m = next.(Model)

	if m.waiting {
		t.Error("Esc should have stopped the waiting state")
	}
	if !m.interrupting {
		t.Error("Esc should have set the interrupting guard for the incoming stream error")
	}
	if m.activeStream != nil {
		t.Error("Esc should have cleared the active stream after closing it")
	}
	if m.messages[0].streaming {
		t.Error("the tail message should no longer be streaming after interrupt")
	}
	found := false
	for _, n := range m.messages[0].Notices {
		if strings.Contains(n, "interrupted") {
			found = true
		}
	}
	if !found {
		t.Errorf("interrupted turn should carry an 'interrupted' notice, got %+v", m.messages[0].Notices)
	}
}

// TestInterruptingSwallowsFinalStreamError confirms the error/EOF the
// closed stream produces after an interrupt is swallowed (no scary error
// surfaced for a cancel the user asked for).
func TestInterruptingSwallowsFinalStreamError(t *testing.T) {
	m := testModel(t)
	m.messages = []chatMessage{{Role: "assistant"}}
	m.interrupting = true

	next, cmd := m.handleStreamEvent(streamEventMsg{err: errFakeStreamClosed})
	m = next.(Model)
	if m.err != nil {
		t.Errorf("interrupted stream error should be swallowed, got m.err = %v", m.err)
	}
	if m.interrupting {
		t.Error("interrupting flag should reset after swallowing the final error")
	}
	if cmd != nil {
		t.Error("no further stream read should be scheduled after an interrupt")
	}
}

// TestEscDoesNotInterruptWhenNotWaiting confirms Esc with no active turn
// is a harmless no-op (doesn't set interrupting or crash on a nil stream).
func TestEscDoesNotInterruptWhenNotWaiting(t *testing.T) {
	m := testModel(t)
	m.waiting = false
	next, _ := m.handleKey(keyMsg("esc"))
	m = next.(Model)
	if m.interrupting {
		t.Error("Esc with no in-flight turn should not enter the interrupting state")
	}
}

var interruptFakeStream daemonclient.MessageStream // zero-value; Close() is nil-safe
var errFakeStreamClosed = errors.New("stream closed")
