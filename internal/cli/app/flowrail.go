package app

import (
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// Directional flow rail (#132): the activity rail animates by data
// direction — glyphs flowing right while model output arrives (deltas/
// reasoning), left while data heads to the model (a call's prompt going
// up, tool results about to be sent back), converging when both are
// fresh at once — and its color is a volume heat ramp. Direction is
// horizontal by necessity and by convention: the indicator is one
// terminal row tall (vertical motion cannot exist there), and RX/TX
// flows read left/right everywhere else in computing.

const (
	// flowActiveWindow is how long after the last byte a direction still
	// counts as active — long enough to bridge chunk gaps in a healthy
	// stream, short enough that the rail goes neutral when work pauses.
	flowActiveWindow = 700 * time.Millisecond
	// flowRateWindow is the accumulation window for the bytes/sec
	// estimate the heat color derives from.
	flowRateWindow = 500 * time.Millisecond
)

// flowHeat* are the volume ramp's buckets, log-spaced because LLM
// throughput spans orders of magnitude. Deliberately ends in hot magenta
// rather than colorDanger's red — red already means *failure* in this
// TUI (open breaker, errors), and a healthy high-throughput stream must
// never read as broken.
var (
	flowHeatGreen  = lipgloss.Color("114") // < 256 B/s — a slow, steady stream
	flowHeatYellow = lipgloss.Color("179") // < 1 KiB/s
	flowHeatOrange = lipgloss.Color("208") // < 4 KiB/s
	flowHeatHot    = lipgloss.Color("205") // >= 4 KiB/s — hot magenta, not danger red
)

func flowHeatColor(bytesPerSec float64) lipgloss.Color {
	switch {
	case bytesPerSec >= 4096:
		return flowHeatHot
	case bytesPerSec >= 1024:
		return flowHeatOrange
	case bytesPerSec >= 256:
		return flowHeatYellow
	default:
		return flowHeatGreen
	}
}

// noteFlowRx records model-output bytes arriving (deltas, reasoning) —
// the rail's rightward direction. Receiving also ends the sending phase
// a route_start opened: the first byte back proves the upload finished.
func (m *Model) noteFlowRx(n int) {
	m.flowRxAt = time.Now()
	m.flowSending = false
	m.noteFlowBytes(n)
}

// noteFlowTx records bytes headed to the model (tool args now, tool
// results on the next call) — the rail's leftward direction.
func (m *Model) noteFlowTx(n int) {
	m.flowTxAt = time.Now()
	m.noteFlowBytes(n)
}

// noteFlowBytes accumulates into the rate window and, once the window
// elapses, folds it into the smoothed bytes/sec estimate the heat color
// reads. 50/50 smoothing: responsive to bursts without flickering
// between adjacent buckets every chunk.
func (m *Model) noteFlowBytes(n int) {
	now := time.Now()
	if m.flowRateAt.IsZero() {
		m.flowRateAt = now
	}
	m.flowBytes += n
	if elapsed := now.Sub(m.flowRateAt); elapsed >= flowRateWindow {
		instant := float64(m.flowBytes) / elapsed.Seconds()
		m.flowRate = 0.5*m.flowRate + 0.5*instant
		m.flowBytes = 0
		m.flowRateAt = now
	}
}

// flowDirections reports which directions are currently active.
func (m Model) flowDirections() (rx, tx bool) {
	rx = !m.flowRxAt.IsZero() && time.Since(m.flowRxAt) < flowActiveWindow
	tx = m.flowSending || (!m.flowTxAt.IsZero() && time.Since(m.flowTxAt) < flowActiveWindow)
	return rx, tx
}

// renderFlowRail draws the directional rail for the active directions.
// ok=false means no direction is fresh — the caller falls back to the
// neutral cycling pulse.
func (m Model) renderFlowRail() (string, bool) {
	rx, tx := m.flowDirections()
	if !rx && !tx {
		return "", false
	}
	const nodes = 7
	heat := lipgloss.NewStyle().Foreground(flowHeatColor(m.flowRate)).Bold(true)
	step := positiveModulo(m.animFrame/activeStepFrames, nodes)

	glyphs := make([]string, nodes)
	for i := range glyphs {
		glyphs[i] = styleFaintTrack.Render("·")
	}
	put := func(i int, g string) {
		if i >= 0 && i < nodes {
			glyphs[i] = heat.Render(g)
		}
	}

	switch {
	case rx && tx:
		// Convergence: both ends run toward the center and meet there.
		off := positiveModulo(m.animFrame/activeStepFrames, nodes/2+1)
		put(off, "»")
		put(nodes-1-off, "«")
		if off == nodes/2 {
			glyphs[nodes/2] = heat.Render("◆")
		}
	case rx:
		// Model output flowing in: a two-glyph cluster sweeping right.
		put(step-1, "»")
		put(step, "»")
	default:
		// Data heading to the model: the mirror, sweeping left.
		pos := nodes - 1 - step
		put(pos+1, "«")
		put(pos, "«")
	}
	return "◜" + strings.Join(glyphs, "") + "◝", true
}
