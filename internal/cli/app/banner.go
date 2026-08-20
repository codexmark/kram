package app

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// kramKRows is the supplied retracting-legs K, preserved character-for-
// character from KRAM_K_legs_retracted_ascii.txt. The uneven line lengths are
// intentional: every row is padded to kramKWidth before the central wordmark
// so the right-hand composition always starts in the same column.
var kramKRows = []string{
	"███   ███            ╱██KRAM",
	"█   █████          ╱██KRAM",
	"██   ████        ╱██KRAM",
	"███   ███      ╱██KRAM",
	"████   ██KRAM ──────",
	"██   ████KRAM ────",
	"███   ███KRAM ─",
	"████   ██      ╲██KRAM",
	"█████   █        ╲██KRAM",
	"███   ███          ╲██KRAM",
	"█   █████            ╲██KRAM",
}

// kramWordmarkRows is the ANSI Shadow KRAM wordmark visible in the reference
// image. Keep the box-drawing corners and spacing exact: the outline is part
// of each glyph, not a second decorative layer.
var kramWordmarkRows = []string{
	"██╗  ██╗██████╗  █████╗ ███╗   ███╗",
	"██║ ██╔╝██╔══██╗██╔══██╗████╗ ████║",
	"█████╔╝ ██████╔╝███████║██╔████╔██║",
	"██╔═██╗ ██╔══██╗██╔══██║██║╚██╔╝██║",
	"██║  ██╗██║  ██║██║  ██║██║ ╚═╝ ██║",
	"╚═╝  ╚═╝╚═╝  ╚═╝╚═╝  ╚═╝╚═╝     ╚═╝",
}

const (
	kramTagline = "KNOWLEDGE · REASONING · AUGMENTED · MIDDLEWARE"

	kramKWidth        = 28
	kramWordmarkWidth = 35
	kramTaglineWidth  = 46
	kramLogoGap       = 4

	// Full composition: 28-column K + four-column gap + 46-column tagline.
	// Six columns cover the rounded border and two cells of horizontal padding
	// on each side. At narrower widths the center wordmark remains available
	// without corrupting or wrapping the supplied K.
	bannerFullThreshold    = kramKWidth + kramLogoGap + kramTaglineWidth + 6
	bannerCompactThreshold = kramTaglineWidth + 6
)

type bannerRGB struct{ r, g, b int }

var (
	bannerPurple     = bannerRGB{120, 66, 255} // #7842FF, left edge in the reference
	bannerBlue       = bannerRGB{70, 113, 255} // #4671FF, central wordmark left edge
	bannerCyan       = bannerRGB{2, 236, 255}  // #02ECFF, right edge in the reference
	bannerBackground = bannerRGB{13, 15, 20}   // #0D0F14
	bannerBorder     = bannerRGB{42, 48, 59}   // #2A303B
)

type bannerAnimation struct {
	reveal     float64
	fade       float64
	totalWidth int
}

// renderBanner draws the supplied identity mark inside the same dark rounded
// terminal panel as the reference. Wide terminals get the complete 11-row
// composition; medium terminals retain the exact central wordmark and motto;
// narrow terminals receive a non-wrapping KRAM fallback.
func renderBanner(width int) string {
	return renderAnimatedBanner(width, 1, 0)
}

func renderAnimatedBanner(width int, reveal, fade float64) string {
	var rows []string
	animation := bannerAnimation{reveal: reveal, fade: fade}
	switch {
	case width >= bannerFullThreshold:
		animation.totalWidth = kramKWidth + kramLogoGap + kramTaglineWidth
		rows = renderFullBannerRows(animation)
	case width >= bannerCompactThreshold:
		animation.totalWidth = kramTaglineWidth
		rows = renderCompactBannerRows(animation)
	default:
		animation.totalWidth = 4
		rows = []string{gradientTextAnimated("KRAM", bannerPurple, bannerCyan, 4, 0, 0, animation)}
	}

	border := blendBannerColor(bannerBorder, bannerBackground, fade).hex()
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(border)).
		Background(colorBannerBackground).
		Padding(0, 2)
	// Lip Gloss Width includes padding but not the border, so subtract only
	// the two border cells to make the rendered splash exactly terminal-wide.
	if styleWidth := width - box.GetHorizontalBorderSize(); styleWidth > 0 {
		box = box.Width(styleWidth)
	}
	return box.Render(strings.Join(rows, "\n"))
}

func renderFullBannerRows(animation bannerAnimation) []string {
	plain := fullBannerPlainRows()
	rows := make([]string, len(plain))
	for i, row := range plain {
		left := gradientTextAnimated(row.left, bannerPurple, bannerBlue, kramKWidth, 0, i, animation)
		left += strings.Repeat(" ", kramKWidth-lipgloss.Width(row.left)+kramLogoGap)
		right := ""
		switch {
		case row.wordmark != "":
			right = gradientTextAnimated(row.wordmark, bannerBlue, bannerCyan, kramWordmarkWidth, kramKWidth+kramLogoGap, i, animation)
		case row.tagline != "":
			right = gradientTextAnimated(row.tagline, bannerBlue, bannerCyan, kramTaglineWidth, kramKWidth+kramLogoGap, i, animation)
		}
		rows[i] = left + right
	}
	return rows
}

func renderCompactBannerRows(animation bannerAnimation) []string {
	rows := make([]string, 0, len(kramWordmarkRows)+2)
	for i, row := range kramWordmarkRows {
		rows = append(rows, gradientTextAnimated(row, bannerBlue, bannerCyan, kramWordmarkWidth, 0, i, animation))
	}
	rows = append(rows, "", gradientTextAnimated(kramTagline, bannerBlue, bannerCyan, kramTaglineWidth, 0, len(kramWordmarkRows)+1, animation))
	return rows
}

type bannerPlainRow struct {
	left     string
	wordmark string
	tagline  string
}

// fullBannerPlainRows is the alignment contract, kept free of ANSI so tests
// can compare the actual cells instead of escape sequences. The motto sits on
// row nine, aligned with the ninth row of the K exactly as in the reference.
func fullBannerPlainRows() []bannerPlainRow {
	rows := make([]bannerPlainRow, len(kramKRows))
	for i, left := range kramKRows {
		rows[i].left = left
		if i < len(kramWordmarkRows) {
			rows[i].wordmark = kramWordmarkRows[i]
		}
	}
	rows[8].tagline = kramTagline
	return rows
}

// gradientText applies a stable horizontal RGB gradient by cell position.
// span is the reference component width, not len(text), so shorter K rows keep
// the same color at the same column instead of stretching their own gradient.
func gradientText(text string, from, to bannerRGB, span int) string {
	return gradientTextAnimated(text, from, to, span, 0, 0, bannerAnimation{reveal: 1, totalWidth: span})
}

func gradientTextAnimated(text string, from, to bannerRGB, span, offset, row int, animation bannerAnimation) string {
	runes := []rune(text)
	if len(runes) == 0 {
		return ""
	}
	var out strings.Builder
	for i, r := range runes {
		if r == ' ' || !animation.bannerCellVisible(offset+i, row) {
			// Invisible cells remain occupied so the wipe never moves adjacent
			// glyphs or changes the panel's geometry between frames.
			out.WriteRune(' ')
			continue
		}
		color := blendBannerColor(bannerGradientRGB(from, to, i, span), bannerBackground, animation.fade).hex()
		out.WriteString(lipgloss.NewStyle().
			Foreground(lipgloss.Color(color)).
			Background(colorBannerBackground).
			Bold(true).
			Render(string(r)))
	}
	return out.String()
}

func bannerGradientColor(from, to bannerRGB, column, span int) string {
	return bannerGradientRGB(from, to, column, span).hex()
}

func bannerGradientRGB(from, to bannerRGB, column, span int) bannerRGB {
	if span <= 1 {
		span = 1
	}
	if column < 0 {
		column = 0
	}
	if column >= span {
		column = span - 1
	}
	denominator := span - 1
	if denominator == 0 {
		denominator = 1
	}
	blend := func(a, b int) int {
		return a + (b-a)*column/denominator
	}
	return bannerRGB{blend(from.r, to.r), blend(from.g, to.g), blend(from.b, to.b)}
}

func (c bannerRGB) hex() string {
	return fmt.Sprintf("#%02X%02X%02X", c.r, c.g, c.b)
}

func blendBannerColor(from, to bannerRGB, amount float64) bannerRGB {
	if amount < 0 {
		amount = 0
	}
	if amount > 1 {
		amount = 1
	}
	blend := func(a, b int) int { return a + int(float64(b-a)*amount) }
	return bannerRGB{blend(from.r, to.r), blend(from.g, to.g), blend(from.b, to.b)}
}

func (a bannerAnimation) bannerCellVisible(column, row int) bool {
	if a.totalWidth <= 0 {
		return true
	}
	if float64(column) >= a.reveal*float64(a.totalWidth) {
		return false
	}
	if a.fade <= 0 {
		return true
	}
	// A deterministic cell dissolve remains visible even on terminals that
	// quantize away the true-color fade. At fade=1 every glyph is gone.
	threshold := int(a.fade * 100)
	return (column*37+row*17)%100 >= threshold
}
