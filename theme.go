package main

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// ---------------------------------------------------------------- theme

type Theme struct {
	Title, Subtitle             lipgloss.Style
	Success, Warn, Danger, Info lipgloss.Style
	Muted, Chip, Box, Key, Path lipgloss.Style
	Border                      lipgloss.Style
}

func NewTheme(color bool) *Theme {
	if !color {
		p := lipgloss.NewStyle()
		return &Theme{
			Title: p.Bold(true), Subtitle: p.Bold(true),
			Success: p, Warn: p, Danger: p, Info: p, Muted: p,
			Chip: p, Key: p, Path: p, Border: p,
			Box: p.Border(lipgloss.NormalBorder()).Padding(1, 2),
		}
	}
	return &Theme{
		Title: lipgloss.NewStyle().Bold(true).
			Foreground(lipgloss.Color("#FAFAFA")).
			Background(lipgloss.Color("#7D56F4")).
			Padding(0, 2),
		Subtitle: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#EE6FF8")),
		Success:  lipgloss.NewStyle().Foreground(lipgloss.Color("#04B575")),
		Warn:     lipgloss.NewStyle().Foreground(lipgloss.Color("#F5A623")),
		Danger:   lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FF5F87")),
		Info:     lipgloss.NewStyle().Foreground(lipgloss.Color("#00BFFF")),
		Muted:    lipgloss.NewStyle().Foreground(lipgloss.Color("#737373")),
		Path:     lipgloss.NewStyle().Foreground(lipgloss.Color("#B4B4B4")),
		Key: lipgloss.NewStyle().Bold(true).
			Foreground(lipgloss.Color("#1A1A1A")).
			Background(lipgloss.Color("#00BFFF")).Padding(0, 1),
		Chip: lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FAFAFA")).
			Background(lipgloss.Color("#434343")).Padding(0, 1),
		Border: lipgloss.NewStyle().Foreground(lipgloss.Color("#5A5A5A")),
		Box: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#5A5A5A")).
			Padding(1, 2),
	}
}

func (t *Theme) Bar(cur, budget, width int) string {
	if budget <= 0 {
		budget = 1
	}
	ratio := float64(cur) / float64(budget)
	filled := int(ratio * float64(width))
	if filled > width {
		filled = width
	}
	if filled < 0 {
		filled = 0
	}
	st := t.Success
	switch {
	case ratio >= 1.0:
		st = t.Danger
	case ratio >= 0.85:
		st = t.Warn
	}
	return st.Render(strings.Repeat("█", filled)) +
		t.Muted.Render(strings.Repeat("░", width-filled))
}

func (t *Theme) Rule(w int) string { return t.Border.Render(strings.Repeat("─", w)) }
