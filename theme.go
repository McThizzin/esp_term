package main

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Theme is a small palette. Everything in the UI is derived from it, so
// adding a theme is just adding an entry to the map below.
type Theme struct {
	Name                                                      string
	Border, Accent, Text, Dim, BadgeFg                        lipgloss.Color
	Red, Yellow, Green, Cyan, Blue, Mauve, Pink, Orange, Teal lipgloss.Color
}

var themeOrder = []string{"mocha", "tokyo", "gruvbox", "nord"}

var themes = map[string]Theme{
	"mocha": { // Catppuccin Mocha
		Name: "mocha", Border: "#6c7086", Accent: "#89b4fa", Text: "#cdd6f4", Dim: "#7f849c", BadgeFg: "#1e1e2e",
		Red: "#f38ba8", Yellow: "#f9e2af", Green: "#a6e3a1", Cyan: "#89dceb", Blue: "#89b4fa",
		Mauve: "#cba6f7", Pink: "#f5c2e7", Orange: "#fab387", Teal: "#94e2d5",
	},
	"tokyo": { // Tokyo Night
		Name: "tokyo", Border: "#3b4261", Accent: "#7aa2f7", Text: "#c0caf5", Dim: "#565f89", BadgeFg: "#1a1b26",
		Red: "#f7768e", Yellow: "#e0af68", Green: "#9ece6a", Cyan: "#7dcfff", Blue: "#7aa2f7",
		Mauve: "#bb9af7", Pink: "#ff9ac1", Orange: "#ff9e64", Teal: "#73daca",
	},
	"gruvbox": {
		Name: "gruvbox", Border: "#665c54", Accent: "#fabd2f", Text: "#ebdbb2", Dim: "#928374", BadgeFg: "#282828",
		Red: "#fb4934", Yellow: "#fabd2f", Green: "#b8bb26", Cyan: "#8ec07c", Blue: "#83a598",
		Mauve: "#d3869b", Pink: "#d3869b", Orange: "#fe8019", Teal: "#8ec07c",
	},
	"nord": {
		Name: "nord", Border: "#4c566a", Accent: "#88c0d0", Text: "#eceff4", Dim: "#616e88", BadgeFg: "#2e3440",
		Red: "#bf616a", Yellow: "#ebcb8b", Green: "#a3be8c", Cyan: "#8fbcbb", Blue: "#81a1c1",
		Mauve: "#b48ead", Pink: "#b48ead", Orange: "#d08770", Teal: "#8fbcbb",
	},
}

// color maps a user-facing color name (used by /hl) to a theme color.
func (t Theme) color(name string) (lipgloss.Color, bool) {
	switch strings.ToLower(name) {
	case "red":
		return t.Red, true
	case "yellow":
		return t.Yellow, true
	case "green":
		return t.Green, true
	case "cyan":
		return t.Cyan, true
	case "blue":
		return t.Blue, true
	case "mauve", "purple":
		return t.Mauve, true
	case "pink":
		return t.Pink, true
	case "orange":
		return t.Orange, true
	case "teal":
		return t.Teal, true
	}
	return "", false
}

// default rotation used when /hl is given no color
var hlPalette = []string{"yellow", "pink", "teal", "orange", "mauve", "blue", "green", "red"}

type Styles struct {
	Dim, App, Tx, Alert, Plain, Accent, Text lipgloss.Style
	DecAddr, DecFunc, DecLoc                 lipgloss.Style
	Level                                    map[byte]lipgloss.Style
}

func newStyles(t Theme) Styles {
	fg := func(c lipgloss.Color) lipgloss.Style { return lipgloss.NewStyle().Foreground(c) }
	return Styles{
		Dim:     fg(t.Dim),
		App:     fg(t.Mauve),
		Tx:      fg(t.Blue),
		Alert:   fg(t.Red).Bold(true),
		Plain:   fg(t.Text),
		Accent:  fg(t.Accent).Bold(true),
		Text:    fg(t.Text),
		DecAddr: fg(t.Dim),
		DecFunc: fg(t.Green).Bold(true),
		DecLoc:  fg(t.Blue),
		Level: map[byte]lipgloss.Style{
			'E': fg(t.Red),
			'W': fg(t.Yellow),
			'I': fg(t.Green),
			'D': fg(t.Cyan),
			'V': fg(t.Dim),
		},
	}
}

// Icons: plain by default, Nerd Font glyphs with -nerd.
type Icons struct{ Output, Device, Filter, Decoder, Highlight, Input, Help string }

var asciiIcons = Icons{}
var nerdIcons = Icons{
	Output:    "\uf120",
	Device:    "\uf2db",
	Filter:    "\uf0b0",
	Decoder:   "\uf188",
	Highlight: "\uf040",
	Input:     "\uf11c",
	Help:      "\uf059",
}

func titled(icon, name string) string {
	if icon == "" {
		return name
	}
	return icon + " " + name
}
