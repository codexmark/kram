package app

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	splashRevealFrames = 18
	splashHoldFrames   = 6
	splashFadeFrames   = 12
	splashTotalFrames  = splashRevealFrames + splashHoldFrames + splashFadeFrames
	splashFrameTime    = 45 * time.Millisecond
)

type splashTickMsg struct{}

func splashTickCmd() tea.Cmd {
	return tea.Tick(splashFrameTime, func(time.Time) tea.Msg { return splashTickMsg{} })
}

// splashAnimation converts the deterministic frame counter into two separate
// motions: a left-to-right reveal, followed by a short hold and a dissolve.
// Keeping this frame-based makes the boot sequence testable and independent
// from render speed; Bubble Tea merely supplies the clock ticks.
func splashAnimation(frame int) (reveal, fade float64) {
	switch {
	case frame <= 0:
		return 0, 0
	case frame < splashRevealFrames:
		reveal = float64(frame) / float64(splashRevealFrames)
		// Smoothstep prevents the wipe from snapping into and out of motion.
		reveal = reveal * reveal * (3 - 2*reveal)
		return reveal, 0
	case frame < splashRevealFrames+splashHoldFrames:
		return 1, 0
	default:
		fadeFrame := frame - splashRevealFrames - splashHoldFrames + 1
		fade = float64(fadeFrame) / float64(splashFadeFrames)
		if fade > 1 {
			fade = 1
		}
		return 1, fade
	}
}

func (m Model) renderBootSplash() string {
	reveal, fade := splashAnimation(m.splashFrame)
	banner := renderAnimatedBanner(m.width, reveal, fade)
	return lipgloss.Place(
		m.width, m.height,
		lipgloss.Center, lipgloss.Center,
		banner,
		lipgloss.WithWhitespaceBackground(colorBannerBackground),
	)
}

func (m Model) finishSplash() (tea.Model, tea.Cmd) {
	m.phase = m.splashTarget
	if m.ready && !m.wizardMode {
		m.syncViewportSize()
	}
	return m, m.phaseInitCmd(m.phase)
}
