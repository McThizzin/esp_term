package main

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

var (
	ansiRe  = regexp.MustCompile(`\x1b(\[[0-?]*[ -/]*[@-~]|[@-Z\\-_])`)
	logRe   = regexp.MustCompile(`^([EWIDV]) \(\d+\) `)
	alertRe = regexp.MustCompile(`(?i)Guru Meditation|abort\(\)|Backtrace:|assert failed|Task watchdog|Brownout|rst:0x[0-9a-f]+ \((TG\dWDT|RTCWDT|SW_CPU|WDT)|panic'ed`)
	decRe   = regexp.MustCompile(`^(\(inlined by\)\s+)?(?:(0x[0-9a-fA-F]+):\s+)?(.+?) at (.+)$`)
)

// clean strips escape sequences / control chars from device output. We apply
// our own coloring instead, so device colors can't corrupt the layout.
func clean(s string) string {
	s = ansiRe.ReplaceAllString(s, "")
	s = strings.ToValidUTF8(s, "\uFFFD")
	return strings.Map(func(r rune) rune {
		if r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

func human(n float64) string {
	units := []string{"B", "KB", "MB", "GB"}
	i := 0
	for n >= 1024 && i < len(units)-1 {
		n /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%.0f B", n)
	}
	return fmt.Sprintf("%.1f %s", n, units[i])
}

// fit truncates or pads s to exactly w cells (ANSI-aware).
func fit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) > w {
		s = lipgloss.NewStyle().MaxWidth(w).Render(s) + "\x1b[0m"
	}
	if pad := w - lipgloss.Width(s); pad > 0 {
		s += strings.Repeat(" ", pad)
	}
	return s
}

// ------------------------------------------------------- line rendering ----

func (m *model) classify(text string) lipgloss.Style {
	if alertRe.MatchString(text) {
		return m.st.Alert
	}
	if mm := logRe.FindStringSubmatch(text); mm != nil {
		return m.st.Level[mm[1][0]]
	}
	return m.st.Plain
}

func (m *model) hlStyle(name string) lipgloss.Style {
	col, ok := m.th.color(name)
	if !ok {
		col = m.th.Yellow
	}
	return lipgloss.NewStyle().Background(col).Foreground(m.th.BadgeFg).Bold(true)
}

func (m *model) renderDev(text string) string {
	base := m.classify(text)
	if len(m.hls) == 0 {
		return base.Render(text)
	}
	type rng struct {
		s, e  int
		color string
	}
	var rs []rng
	for _, h := range m.hls {
		for _, loc := range h.re.FindAllStringIndex(text, -1) {
			if loc[1] == loc[0] {
				continue
			}
			overlap := false
			for _, r := range rs {
				if loc[0] < r.e && loc[1] > r.s {
					overlap = true
					break
				}
			}
			if !overlap {
				rs = append(rs, rng{loc[0], loc[1], h.color})
			}
		}
	}
	if len(rs) == 0 {
		return base.Render(text)
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].s < rs[j].s })
	var b strings.Builder
	pos := 0
	for _, r := range rs {
		if r.s > pos {
			b.WriteString(base.Render(text[pos:r.s]))
		}
		b.WriteString(m.hlStyle(r.color).Render(text[r.s:r.e]))
		pos = r.e
	}
	if pos < len(text) {
		b.WriteString(base.Render(text[pos:]))
	}
	return b.String()
}

func (m *model) renderDecode(text string) string {
	t := strings.TrimSpace(text)
	mm := decRe.FindStringSubmatch(t)
	if mm == nil {
		return m.st.Dim.Render("  ↳ ") + m.st.DecFunc.Render(t)
	}
	var b strings.Builder
	b.WriteString(m.st.Dim.Render("  ↳ "))
	if mm[1] != "" {
		b.WriteString(m.st.Dim.Render("inlined by "))
	}
	if mm[2] != "" {
		b.WriteString(m.st.DecAddr.Render(mm[2]) + " ")
	}
	b.WriteString(m.st.DecFunc.Render(mm[3]))
	b.WriteString(m.st.Dim.Render(" at "))
	b.WriteString(m.st.DecLoc.Render(mm[4]))
	return b.String()
}

func (m *model) renderLine(l *line) string {
	var body string
	switch l.kind {
	case kindApp:
		body = m.st.App.Render("◆ " + l.text)
	case kindTx:
		body = m.st.Tx.Render("» " + l.text)
	case kindDecode:
		body = m.renderDecode(l.text)
	default:
		body = m.renderDev(l.text)
	}
	if m.timestamps && l.stamp != "" && l.kind != kindDecode {
		body = m.st.Dim.Render(l.stamp) + " " + body
	}
	if w := m.vp.Width; w > 0 {
		return lipgloss.NewStyle().Width(w).Render(body)
	}
	return body
}

// ------------------------------------------------------------- panels ------

// panel draws a rounded box with the title embedded in the top border.
// h includes the border rows; w includes the border columns.
func (m *model) panel(title, right string, body []string, w, h int, focused bool) string {
	if w < 6 || h < 2 {
		return ""
	}
	bc, titleFg := m.th.Border, m.th.Text
	if focused {
		bc, titleFg = m.th.Accent, m.th.Accent
	}
	bs := lipgloss.NewStyle().Foreground(bc)
	ts := lipgloss.NewStyle().Bold(true).Foreground(titleFg)
	iw := w - 2

	tw := lipgloss.Width(title)
	rightW, rightStr := 1, bs.Render("╮")
	if right != "" {
		rightW = lipgloss.Width(right) + 4
		rightStr = bs.Render(" ") + right + bs.Render(" ─╮")
	}
	fill := w - (4 + tw) - rightW
	if fill < 0 && right != "" {
		rightW, rightStr = 1, bs.Render("╮")
		fill = w - (4 + tw) - rightW
	}
	if fill < 0 {
		title = lipgloss.NewStyle().MaxWidth(max(w-5, 1)).Render(title)
		tw = lipgloss.Width(title)
		fill = max(w-(4+tw)-rightW, 0)
	}

	var sb strings.Builder
	sb.WriteString(bs.Render("╭─ ") + ts.Render(title) + bs.Render(" "+strings.Repeat("─", fill)) + rightStr)
	for i := 0; i < h-2; i++ {
		l := ""
		if i < len(body) {
			l = body[i]
		}
		sb.WriteString("\n" + bs.Render("│") + fit(l, iw) + bs.Render("│"))
	}
	sb.WriteString("\n" + bs.Render("╰"+strings.Repeat("─", iw)+"╯"))
	return sb.String()
}

func (m *model) mainPanel() string {
	var status string
	switch {
	case m.paused:
		status = lipgloss.NewStyle().Foreground(m.th.Yellow).Render(fmt.Sprintf("⏸ paused +%d", m.missed))
	case !m.vp.AtBottom():
		status = m.st.Dim.Render(fmt.Sprintf("↑ %d%%", int(m.vp.ScrollPercent()*100)))
	default:
		status = lipgloss.NewStyle().Foreground(m.th.Green).Render("● live")
	}
	right := status
	if m.filter.re != nil {
		right = m.st.Dim.Render(fmt.Sprintf("%d/%d lines", m.shown, len(m.lines))) + "  " + status
	}
	body := strings.Split(m.vp.View(), "\n")
	return m.panel(titled(m.icons.Output, "Serial Output"), right, body, m.mainW, m.mainH, m.focus == focusOutput)
}

func (m *model) inputPanel() string {
	right := m.st.Dim.Render("↵ send")
	return m.panel(titled(m.icons.Input, "Input"), right, []string{" " + m.input.View()}, m.width, 3, m.focus == focusInput)
}

func (m *model) footer() string {
	type hint struct{ k, d string }
	var hs []hint
	if m.focus == focusOutput {
		hs = []hint{{"Tab", "input"}, {"↑↓", "scroll"}, {"g/G", "top/end"}, {"p", "pause"}, {"c", "clear"}, {"?", "help"}}
	} else {
		hs = []hint{{"Tab", "output"}, {"PgUp/Dn", "scroll"}, {"F1", "help"}, {"F2", "sidebar"}, {"F3", "time"}, {"F4", "pause"},
			{"F5", "reset"}, {"F6", "boot"}, {"^C", "quit"}}
	}
	parts := make([]string, len(hs))
	for i, h := range hs {
		parts[i] = m.st.Accent.Render(h.k) + " " + m.st.Dim.Render(h.d)
	}
	return fit(" "+strings.Join(parts, m.st.Dim.Render("  ·  ")), m.width)
}

// ------------------------------------------------------------- sidebar -----

type sbPanel struct {
	title string
	lines []string
}

func (m *model) chip(text string, bg lipgloss.Color) string {
	return lipgloss.NewStyle().Background(bg).Foreground(m.th.BadgeFg).Bold(true).Padding(0, 1).Render(text)
}

func (m *model) kvs(label, styled string) string {
	return " " + m.st.Dim.Render(fmt.Sprintf("%-6s", label)) + styled
}

func (m *model) kv(label, val string) string { return m.kvs(label, m.st.Text.Render(val)) }

func (m *model) devPanel() sbPanel {
	chip := m.chip("○ offline", m.th.Red)
	up := "—"
	if m.connected {
		chip = m.chip("● connected", m.th.Green)
		d := time.Since(m.connectedAt).Round(time.Second)
		up = fmt.Sprintf("%02d:%02d:%02d", int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60)
	}
	ts := "off"
	if m.timestamps {
		ts = "on"
	}
	return sbPanel{titled(m.icons.Device, "Device"), []string{
		" " + chip,
		m.kv("port", filepath.Base(m.link.name)),
		m.kv("baud", strconv.Itoa(m.link.getBaud())),
		m.kv("rx", fmt.Sprintf("%s · %s/s", human(float64(m.link.rx.Load())), human(m.rxRate))),
		m.kv("tx", human(float64(m.link.tx.Load()))),
		m.kv("up", up),
		m.kv("time", ts),
	}}
}

func (m *model) filterPanel() sbPanel {
	pat, mode := m.st.Dim.Render("none"), m.st.Dim.Render("—")
	if m.filter.re != nil {
		pat = m.st.Text.Render("/" + m.filter.src + "/")
		mode = m.st.Text.Render("include")
		if m.filter.invert {
			mode = m.st.Text.Render("exclude")
		}
	}
	return sbPanel{titled(m.icons.Filter, "Filter"), []string{
		m.kvs("regex", pat),
		m.kvs("mode", mode),
		m.kv("show", fmt.Sprintf("%d/%d", m.shown, len(m.lines))),
	}}
}

func (m *model) decoderPanel() sbPanel {
	elfS, toolS := m.st.Dim.Render("none"), m.st.Dim.Render("—")
	if m.elf != "" {
		elfS = m.st.Text.Render(filepath.Base(m.elf))
		toolS = lipgloss.NewStyle().Foreground(m.th.Red).Render("not found")
		if m.tool != "" {
			toolS = m.st.Text.Render(filepath.Base(m.tool))
		}
	}
	return sbPanel{titled(m.icons.Decoder, "Decoder"), []string{
		m.kvs("elf", elfS),
		m.kvs("tool", toolS),
	}}
}

func (m *model) hlPanel() sbPanel {
	var lines []string
	if len(m.hls) == 0 {
		lines = []string{" " + m.st.Dim.Render("none · /hl <regex>")}
	}
	for i, h := range m.hls {
		col, _ := m.th.color(h.color)
		sw := lipgloss.NewStyle().Foreground(col).Render("■")
		lines = append(lines, fmt.Sprintf(" %s %s %s", m.st.Dim.Render(strconv.Itoa(i+1)), sw, m.st.Text.Render(h.src)))
	}
	return sbPanel{titled(m.icons.Highlight, "Highlights"), lines}
}

func (m *model) sidebar() string {
	panels := []sbPanel{m.devPanel(), m.filterPanel(), m.decoderPanel(), m.hlPanel()}
	rem := m.mainH
	var blocks []string
	for i, p := range panels {
		if rem < 3 {
			break
		}
		h := len(p.lines) + 2
		if i == len(panels)-1 {
			h = rem // last panel soaks up the leftover height
		}
		if h > rem {
			h = rem
		}
		blocks = append(blocks, m.panel(p.title, "", p.lines, m.sbW, h, false))
		rem -= h
	}
	return strings.Join(blocks, "\n")
}

// ------------------------------------------------------------- help --------

func (m *model) helpRows() []string {
	hdr := func(s string) string {
		return " " + lipgloss.NewStyle().Bold(true).Foreground(m.th.Mauve).Render(s)
	}
	row := func(k, d string) string {
		return " " + m.st.Accent.Render(fmt.Sprintf("%-26s", k)) + m.st.Text.Render(d)
	}
	return []string{
		hdr("Keys"),
		row("Tab", "switch focus: input ⇄ output"),
		row("Enter", "send line to the device"),
		row("↑ / ↓", "input history"),
		row("PgUp / PgDn", "scroll output"),
		row("Esc", "jump to bottom / resume"),
		row("F1  F2  F3  F4", "help · sidebar · timestamps · pause"),
		row("F5  F6", "reset chip · enter bootloader"),
		row("Ctrl+C", "quit"),
		row("(output focus) j k g G p c", "scroll · top/end · pause · clear"),
		"",
		hdr("Commands"),
		row("/reset   /boot", "reset chip / enter download mode"),
		row("/baud <rate>", "change baud rate"),
		row("/filter <re>", "only show matching lines"),
		row("/filter -v <re>", "hide matching lines"),
		row("/filter off", "clear the filter"),
		row("/hl <re> [color]", "highlight matches (red yellow green"),
		row("", "cyan blue mauve pink orange teal)"),
		row("/hl rm <n>  |  /hl clear", "remove highlights"),
		row("/elf <path>  |  /elf off", "ELF used to decode backtraces"),
		row("/addr2line <path>", "override the addr2line binary"),
		row("/decode 0x400d.. ...", "decode addresses by hand"),
		row("/ts  /pause  /sidebar", "toggles"),
		row("/theme <name>", "mocha · tokyo · gruvbox · nord"),
		row("/log <file|off>", "log session to a file"),
		row("/eol <lf|crlf|cr|none>", "line ending for sent text"),
		row("/hex 03 0d 0a", "send raw bytes"),
		row("/ports  /clear  /quit", ""),
		"",
		hdr("Notes"),
		row("regexes", "case-insensitive; use (?-i) to turn that off"),
		row("//text", "sends '/text' to the device"),
		row("backtraces", "auto-decoded when an ELF is set"),
	}
}

func (m *model) helpView() string {
	rows := m.helpRows()
	w := max(min(m.width-4, 78), 30)
	h := max(min(len(rows)+2, m.height-2), 4)
	ih := h - 2
	maxOff := max(len(rows)-ih, 0)
	if m.helpOff > maxOff {
		m.helpOff = maxOff
	}
	body := rows[m.helpOff:min(m.helpOff+ih, len(rows))]
	right := m.st.Dim.Render("esc close")
	if maxOff > 0 {
		right = m.st.Dim.Render("↑↓ scroll · esc close")
	}
	p := m.panel(titled(m.icons.Help, "Help"), right, body, w, h, true)
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, p)
}

// ------------------------------------------------------------- View --------

func (m *model) View() string {
	if !m.ready {
		return ""
	}
	if m.showHelp {
		return m.helpView()
	}
	top := m.mainPanel()
	if m.sbW > 0 {
		top = lipgloss.JoinHorizontal(lipgloss.Top, m.sidebar(), top)
	}
	return top + "\n" + m.inputPanel() + "\n" + m.footer()
}
