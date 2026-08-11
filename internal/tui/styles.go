// Lipgloss styles for the TUI. Dark theme — charcoal background, electric
// blue accent, semantic colours for status/error/warning.
package tui

import "github.com/charmbracelet/lipgloss"

var (
	colorBG     = lipgloss.Color("#1a1b26")
	colorFG     = lipgloss.Color("#c0caf5")
	colorAccent = lipgloss.Color("#7aa2f7")
	colorMuted  = lipgloss.Color("#565f89")
	colorOK     = lipgloss.Color("#9ece6a")
	colorWarn   = lipgloss.Color("#e0af68")
	colorErr    = lipgloss.Color("#f7768e")
	colorUser   = lipgloss.Color("#bb9af7")

	styleApp = lipgloss.NewStyle().
			Foreground(colorFG).
			Background(colorBG)

	styleSidebarBorder = lipgloss.NewStyle().
				Border(lipgloss.NormalBorder()).
				BorderForeground(colorMuted).
				Padding(0, 1)

	styleChatBorder = lipgloss.NewStyle().
			Border(lipgloss.NormalBorder()).
			BorderForeground(colorMuted).
			Padding(0, 1)

	styleInputBorder = lipgloss.NewStyle().
				Border(lipgloss.NormalBorder()).
				BorderForeground(colorAccent).
				Padding(0, 1)

	styleHeader  = lipgloss.NewStyle().Foreground(colorAccent).Bold(true)
	styleMuted   = lipgloss.NewStyle().Foreground(colorMuted)
	styleUser    = lipgloss.NewStyle().Foreground(colorUser).Bold(true)
	styleAssist  = lipgloss.NewStyle().Foreground(colorFG)
	styleTool    = lipgloss.NewStyle().Foreground(colorAccent)
	styleToolRes = lipgloss.NewStyle().Foreground(colorMuted)
	styleOK      = lipgloss.NewStyle().Foreground(colorOK)
	styleErr     = lipgloss.NewStyle().Foreground(colorErr).Bold(true)
	styleWarn    = lipgloss.NewStyle().Foreground(colorWarn).Bold(true)
	styleWarnBlk = lipgloss.NewStyle().
			Foreground(colorWarn).
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorWarn).
			Padding(0, 1).
			Margin(1, 0)

	styleApproval = lipgloss.NewStyle().
			Foreground(colorFG).
			Background(colorBG).
			Border(lipgloss.DoubleBorder()).
			BorderForeground(colorWarn).
			Padding(1, 2)
)
