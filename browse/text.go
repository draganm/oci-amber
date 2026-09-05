package browse

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

var (
	styleDim = lipgloss.NewStyle().Faint(true)
	styleHit = lipgloss.NewStyle().Reverse(true)
)

// RenderText renders lines[top:top+height], each shifted left by left
// columns and cut to width, behind right-aligned line numbers. Lines whose
// index is in hits are highlighted. Missing lines at the end are blank, so
// the result always has height lines.
func RenderText(lines []string, top, left, height, width int, hits map[int]bool) string {
	numWidth := len(strconv.Itoa(max(len(lines), 1)))
	out := make([]string, 0, height)
	for i := 0; i < height; i++ {
		n := top + i
		if n < 0 || n >= len(lines) {
			out = append(out, "")
			continue
		}
		num := styleDim.Render(fmt.Sprintf("%*d", numWidth, n+1))
		body := truncate(cutLeft(lines[n], left), max(width-numWidth-1, 0))
		if hits[n] {
			body = styleHit.Render(body)
		}
		out = append(out, num+" "+body)
	}
	return strings.Join(out, "\n")
}

// truncate cuts s to width cells; ANSI sequences are kept intact.
func truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= width {
		return s
	}
	return lipgloss.NewStyle().MaxWidth(width).Render(s)
}

// padRight pads s with spaces to width cells.
func padRight(s string, width int) string {
	if n := width - lipgloss.Width(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// cutLeft drops the first n runes of s.
func cutLeft(s string, n int) string {
	if n <= 0 {
		return s
	}
	rs := []rune(s)
	if n >= len(rs) {
		return ""
	}
	return string(rs[n:])
}
