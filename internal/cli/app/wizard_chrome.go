// Shared visual chrome for the first-run wizard: every step renders
// through renderWizardFrame so the whole flow reads as one designed
// surface — gradient wordmark, step trail, a centered bordered card and
// a consistent key bar — instead of eight ad-hoc top-left text blocks.
// The palette stays Kram's own (see styles.go): subtle chrome, semantic
// colors, the identity gradient reserved for the wordmark.
package app

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// wizardStepNames drive the step trail, indexed by step-1. They are
// deliberately short — the wide-terminal trail shows all eight.
var wizardStepNames = [8]string{
	"Welcome", "Projects", "Providers", "Routing",
	"Permissions", "Tools", "Check", "Ready",
}

const (
	// wizardCardMaxWidth keeps the card a comfortable reading measure on
	// large terminals instead of stretching edge to edge.
	wizardCardMaxWidth = 72
	// wizardWideCardMaxWidth is for list-heavy steps (Providers) whose
	// rows carry URLs and ping details that would wrap badly at 72.
	wizardWideCardMaxWidth = 100
	// wizardTrailNamesMinWidth is the terminal width from which the trail
	// can afford full step names; below it, dots plus "step N/8 · Name".
	wizardTrailNamesMinWidth = 100
)

var (
	styleWizardCard = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorFaint).
			Padding(1, 3)
	styleWizardTitle = lipgloss.NewStyle().Foreground(colorText).Bold(true)
	styleWizardKey   = lipgloss.NewStyle().Foreground(colorAccent)
	styleWizardDim   = lipgloss.NewStyle().Foreground(colorMuted)
)

// wizardWordmark renders the small KRAM wordmark with the banner's
// identity gradient — foreground only, no background block, so it sits
// naturally above the card rather than as a floating chip.
func wizardWordmark() string {
	const word = "KRAM"
	var b strings.Builder
	for i, r := range word {
		color := bannerGradientColor(bannerPurple, bannerCyan, i, len(word))
		b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(color)).Bold(true).Render(string(r)))
	}
	b.WriteString(styleWizardDim.Render(" · setup"))
	return b.String()
}

// renderWizardTrail shows where the user is in the 8-step flow: done
// steps in the success color, the current one highlighted in the accent,
// upcoming ones faint. Wide terminals get full step names; narrow ones a
// dot trail plus "step N/8 · Name" so position is never lost.
func renderWizardTrail(step, width int) string {
	if step < 1 {
		step = 1
	}
	if step > len(wizardStepNames) {
		step = len(wizardStepNames)
	}
	if width >= wizardTrailNamesMinWidth {
		parts := make([]string, 0, len(wizardStepNames))
		for i, name := range wizardStepNames {
			n := i + 1
			switch {
			case n < step:
				parts = append(parts, styleBadgeOK.Render("✓ "+name))
			case n == step:
				parts = append(parts, styleBadgeAccent.Bold(true).Render("▸ "+name))
			default:
				parts = append(parts, styleHint.Render("○ "+name))
			}
		}
		return strings.Join(parts, "  ")
	}
	var dots strings.Builder
	for i := range wizardStepNames {
		n := i + 1
		switch {
		case n < step:
			dots.WriteString(styleBadgeOK.Render("●"))
		case n == step:
			dots.WriteString(styleBadgeAccent.Render("●"))
		default:
			dots.WriteString(styleHint.Render("○"))
		}
		if i < len(wizardStepNames)-1 {
			dots.WriteString(" ")
		}
	}
	dots.WriteString(styleWizardDim.Render(fmt.Sprintf("   step %d/8 · %s", step, wizardStepNames[step-1])))
	return dots.String()
}

// wizardKey is one entry of the footer key bar: the key itself and what
// pressing it does, e.g. {"enter", "continue"}.
type wizardKey struct{ key, action string }

// wizardKeysChoose is the shared key bar of every list-choice step
// (Routing, Permissions) — one ordering, learned once.
var wizardKeysChoose = []wizardKey{{"↑↓", "choose"}, {"enter", "continue"}, {"esc", "back"}}

// renderWizardKeybar renders the consistent footer — accent keys, muted
// actions, identical spacing on every step. Deliberately styleWizardDim
// rather than the near-invisible styleHint: the key bar is how a first-
// time user learns to drive the wizard, so it must be readable.
func renderWizardKeybar(keys []wizardKey) string {
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, styleWizardKey.Render(k.key)+" "+styleWizardDim.Render(k.action))
	}
	return strings.Join(parts, "   ")
}

// renderWizardOptions renders a vertical choice list as mini-cards: the
// selected option gets an accent bar and bold label, and every option's
// description sits on its own line in a readable color — replacing the
// old single-line "%-14s label desc" cramming.
func renderWizardOptions(labels, descs []string, cursor int) string {
	var b strings.Builder
	for i := range labels {
		if i == cursor {
			bar := styleBadgeAccent.Render("▌ ")
			b.WriteString(bar + styleBadgeAccent.Bold(true).Render(labels[i]) + "\n")
			b.WriteString(bar + styleBody.Render(descs[i]) + "\n")
		} else {
			b.WriteString("  " + styleBody.Render(labels[i]) + "\n")
			b.WriteString("  " + styleWizardDim.Render(descs[i]) + "\n")
		}
		if i < len(labels)-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// renderWizardFrame is the one path every wizard step renders through:
// wordmark, step trail, a bordered card holding the step's title and
// body, and the key bar — all centered in the terminal whenever a real
// size is known (tests and pre-WindowSizeMsg renders fall back to a
// plain left-aligned block). keys == nil omits the key bar, for steps
// whose body already carries context-sensitive hints (Providers).
// maxWidth <= 0 means the default card width.
func (m Model) renderWizardFrame(step int, title, body string, keys []wizardKey, maxWidth int) string {
	if maxWidth <= 0 {
		maxWidth = wizardCardMaxWidth
	}
	cardWidth := maxWidth
	if m.width > 0 && m.width-6 < cardWidth {
		cardWidth = m.width - 6
	}
	if cardWidth < 24 {
		cardWidth = 24
	}

	inner := styleWizardTitle.Render(title) + "\n\n" + strings.TrimRight(body, "\n")
	card := styleWizardCard.Width(cardWidth).Render(inner)

	blocks := []string{
		wizardWordmark(),
		renderWizardTrail(step, m.width),
		"",
		card,
	}
	if len(keys) > 0 {
		blocks = append(blocks, "", renderWizardKeybar(keys))
	}
	content := lipgloss.JoinVertical(lipgloss.Center, blocks...)

	if m.width <= 0 || m.height <= 0 {
		return content
	}
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, content)
}
