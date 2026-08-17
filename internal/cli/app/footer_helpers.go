package app

import (
	"fmt"
	"strings"

	"github.com/codexmark/kram-gateway/internal/openai"
)

var sparkBars = []rune("▁▂▃▄▅▆▇█")

// animatedSparkline is an honest "working" indicator, not a fabricated
// telemetry reading: while a request is in flight there's no per-attempt
// data yet (it only exists once the gateway's response lands), so this
// draws a generic traveling pulse rather than pretending to chart real
// numbers.
func animatedSparkline(frame int) string {
	const width = 6
	var b strings.Builder
	for i := 0; i < width; i++ {
		phase := (frame + i*2) % 16
		h := phase
		if h > 8 {
			h = 16 - h
		}
		idx := h * (len(sparkBars) - 1) / 8
		b.WriteRune(sparkBars[idx])
	}
	return styleBadgeWarn.Render(b.String())
}

// staticSparkline renders the real latency of each attempt made for the
// last completed request, scaled relative to the slowest one.
func staticSparkline(attempts []openai.AttemptInfo) string {
	if len(attempts) == 0 {
		return ""
	}
	var maxMS int64 = 1
	for _, a := range attempts {
		if a.LatencyMS > maxMS {
			maxMS = a.LatencyMS
		}
	}
	var b strings.Builder
	for _, a := range attempts {
		idx := int(a.LatencyMS * int64(len(sparkBars)-1) / maxMS)
		if idx >= len(sparkBars) {
			idx = len(sparkBars) - 1
		}
		bar := string(sparkBars[idx])
		if a.OK {
			b.WriteString(styleBadgeOK.Render(bar))
		} else {
			b.WriteString(styleBadgeBad.Render(bar))
		}
	}
	return b.String()
}

// attemptTrailGlyphs renders one dot per attempt in the real fallback
// trail for the last request: green for the provider that ultimately
// served it, red for anything that failed along the way.
func attemptTrailGlyphs(attempts []openai.AttemptInfo) string {
	if len(attempts) == 0 {
		return ""
	}
	var b strings.Builder
	for i, a := range attempts {
		if i > 0 {
			b.WriteString(" ")
		}
		if a.OK {
			b.WriteString(styleBadgeOK.Render("●"))
		} else {
			b.WriteString(styleBadgeBad.Render("●"))
		}
	}
	return b.String()
}

func lastLatencyMS(attempts []openai.AttemptInfo, providerID string) int64 {
	for i := len(attempts) - 1; i >= 0; i-- {
		if attempts[i].Provider == providerID {
			return attempts[i].LatencyMS
		}
	}
	return -1
}

// lastAssistantTokens formats the token usage of the most recent request,
// or "" if nothing has been sent yet.
func (m Model) lastAssistantTokens() string {
	if m.lastUsage.TotalTokens == 0 {
		return ""
	}
	return fmt.Sprintf("%d tok", m.lastUsage.TotalTokens)
}
