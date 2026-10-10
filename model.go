package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const maxLines = 10000

const (
	kindDev = iota
	kindApp
	kindTx
	kindDecode
)

type focusArea int

const (
	focusInput focusArea = iota
	focusOutput
)

var eols = map[string]string{"lf": "\n", "crlf": "\r\n", "cr": "\r", "none": ""}

type line struct {
	kind    int
	stamp   string
	text    string
	visible bool
}

type filterSpec struct {
	src    string
	re     *regexp.Regexp
	invert bool
}

func (f filterSpec) pass(text string) bool {
	if f.re == nil {
		return true
	}
	return f.re.MatchString(text) != f.invert
}

func parseFilter(rest string) (filterSpec, error) {
	rest = strings.TrimSpace(rest)
	if rest == "" || strings.EqualFold(rest, "off") {
		return filterSpec{}, nil
	}
	invert := false
	switch {
	case strings.HasPrefix(rest, "-v "):
		invert, rest = true, strings.TrimSpace(rest[3:])
	case strings.HasPrefix(rest, "!"):
		invert, rest = true, strings.TrimSpace(rest[1:])
	}
	re, err := regexp.Compile("(?i)" + rest)
	if err != nil {
		return filterSpec{}, err
	}
	return filterSpec{src: rest, re: re, invert: invert}, nil
}

type highlight struct {
	src   string
	re    *regexp.Regexp
	color string
}

type tickMsg time.Time
type decodeMsg struct {
	ref   *line
	lines []string
	err   error
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

type model struct {
	link  *Link
	input textinput.Model
	vp    viewport.Model
	ready bool

	th    Theme
	st    Styles
	icons Icons

	width, height int
	mainW, mainH  int
	sbW           int
	sidebarOn     bool
	focus         focusArea
	showHelp      bool
	helpOff       int

	lines    []*line
	rendered []string
	midline  bool
	paused   bool
	missed   int
	shown    int

	connected   bool
	connectedAt time.Time
	timestamps  bool
	eol         string
	eolName     string

	filter filterSpec
	hls    []highlight
	hlNext int

	elf          string
	tool         string
	toolOverride string

	rxPrev, rxRate float64

	hist    []string
	histIdx int

	logFile *os.File
	logPath string
}

func newModel(link *Link, th Theme, icons Icons, ts bool, eolName string) *model {
	ti := textinput.New()
	ti.Prompt = "❯ "
	ti.Placeholder = "type to send to the device · /help for commands"
	ti.CharLimit = 0
	ti.Focus()

	m := &model{
		link: link, input: ti, th: th, st: newStyles(th), icons: icons,
		sidebarOn: true, timestamps: ts, eol: eols[eolName], eolName: eolName,
	}
	m.applyInputStyle()
	return m
}

func (m *model) applyInputStyle() {
	m.input.PromptStyle = lipgloss.NewStyle().Foreground(m.th.Accent).Bold(true)
	m.input.TextStyle = lipgloss.NewStyle().Foreground(m.th.Text)
	m.input.PlaceholderStyle = lipgloss.NewStyle().Foreground(m.th.Dim)
	m.input.Cursor.Style = lipgloss.NewStyle().Foreground(m.th.Accent)
}

func (m *model) Init() tea.Cmd { return tea.Batch(textinput.Blink, tick()) }

// ------------------------------------------------------------ layout -------

func (m *model) layout() {
	if m.width == 0 {
		return
	}
	m.sbW = 0
	if m.sidebarOn && m.width >= 90 {
		m.sbW = 30
	}
	m.mainW = m.width - m.sbW
	m.mainH = max(m.height-4, 3) // input panel (3) + footer (1)
	vw, vh := max(m.mainW-2, 1), max(m.mainH-2, 1)
	if !m.ready {
		m.vp = viewport.New(vw, vh)
		m.ready = true
	} else {
		m.vp.Width, m.vp.Height = vw, vh
	}
	m.input.Width = max(m.width-4-lipgloss.Width(m.input.Prompt), 5)
	m.rebuild()
}

// ------------------------------------------------------------ lines --------

func stampNow() string { return time.Now().Format("15:04:05.000") }

func (m *model) logWrite(s string) {
	if m.logFile != nil {
		_, _ = m.logFile.WriteString(s)
	}
}

func (m *model) push(l *line) {
	m.lines = append(m.lines, l)
	m.rendered = append(m.rendered, m.renderLine(l))
	if len(m.lines) > maxLines {
		drop := len(m.lines) - maxLines
		m.lines = m.lines[drop:]
		m.rendered = m.rendered[drop:]
	}
}

func (m *model) addApp(text string) {
	m.midline = false
	m.push(&line{kind: kindApp, stamp: stampNow(), text: text, visible: true})
	m.logWrite("[esp_term] " + text + "\n")
}

func (m *model) addTx(text string) {
	m.midline = false
	m.push(&line{kind: kindTx, stamp: stampNow(), text: text, visible: true})
	m.logWrite("» " + text + "\n")
}

func (m *model) addDev(text string, partial bool) *line {
	text = clean(text)
	var l *line
	if m.midline && len(m.lines) > 0 && m.lines[len(m.lines)-1].kind == kindDev {
		l = m.lines[len(m.lines)-1]
		l.text += text
		l.visible = m.filter.pass(l.text)
		m.rendered[len(m.rendered)-1] = m.renderLine(l)
		m.logWrite(text)
	} else {
		st := stampNow()
		l = &line{kind: kindDev, stamp: st, text: text, visible: m.filter.pass(text)}
		m.push(l)
		m.logWrite(st + " " + text)
		if m.paused {
			m.missed++
		}
	}
	if !partial {
		m.logWrite("\n")
	}
	m.midline = partial
	return l
}

func (m *model) insertAfter(ref *line, ls []*line) {
	idx := len(m.lines) - 1
	for i := len(m.lines) - 1; i >= 0; i-- {
		if m.lines[i] == ref {
			idx = i
			break
		}
	}
	rend := make([]string, len(ls))
	for i, l := range ls {
		rend[i] = m.renderLine(l)
	}
	m.lines = slices.Insert(m.lines, idx+1, ls...)
	m.rendered = slices.Insert(m.rendered, idx+1, rend...)
}

func (m *model) refresh() {
	if m.paused {
		return
	}
	m.forceRefresh()
}

func (m *model) forceRefresh() {
	if !m.ready {
		return
	}
	atBottom := m.vp.AtBottom()
	var b strings.Builder
	n := 0
	for i, l := range m.lines {
		if !l.visible {
			continue
		}
		if n > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(m.rendered[i])
		n++
	}
	m.shown = n
	m.vp.SetContent(b.String())
	if atBottom {
		m.vp.GotoBottom()
	}
}

func (m *model) rebuild() {
	for i, l := range m.lines {
		m.rendered[i] = m.renderLine(l)
	}
	m.forceRefresh()
}

// ------------------------------------------------------------ state --------

func (m *model) setFocus(f focusArea) {
	m.focus = f
	if f == focusInput {
		m.input.Focus()
	} else {
		m.input.Blur()
	}
}

func (m *model) togglePause() {
	m.paused = !m.paused
	if !m.paused {
		m.missed = 0
		m.forceRefresh()
		m.vp.GotoBottom()
	}
}

func (m *model) toggleTimestamps() {
	m.timestamps = !m.timestamps
	m.rebuild()
	m.addApp("timestamps " + map[bool]string{true: "on", false: "off"}[m.timestamps])
	m.refresh()
}

func (m *model) setFilter(f filterSpec) {
	m.filter = f
	for _, l := range m.lines {
		if l.kind == kindDev {
			l.visible = f.pass(l.text)
		}
	}
	m.forceRefresh()
	if m.ready {
		m.vp.GotoBottom()
	}
}

func (m *model) addHighlight(src, color string) error {
	re, err := regexp.Compile("(?i)" + src)
	if err != nil {
		return err
	}
	if color == "" {
		color = hlPalette[m.hlNext%len(hlPalette)]
		m.hlNext++
	}
	m.hls = append(m.hls, highlight{src: src, re: re, color: color})
	m.rebuild()
	return nil
}

func (m *model) setELF(path string) {
	if _, err := os.Stat(path); err != nil {
		m.addApp("elf: " + err.Error())
		return
	}
	m.elf = path
	m.refreshTool()
	if m.tool == "" {
		m.addApp("ELF set (" + filepath.Base(path) + "), but no addr2line found - install the ESP toolchain or use /addr2line <path>")
	} else {
		m.addApp("decoding backtraces with " + filepath.Base(m.tool) + " against " + filepath.Base(path))
	}
}

func (m *model) refreshTool() {
	if m.toolOverride != "" {
		m.tool = m.toolOverride
		return
	}
	m.tool = ""
	if m.elf == "" {
		return
	}
	if t, err := findTool(m.elf); err == nil {
		m.tool = t
	}
}

func (m *model) decodeCmd(ref *line, addrs []string) tea.Cmd {
	elfPath, tool := m.elf, m.tool
	return func() tea.Msg {
		lines, err := runAddr2line(tool, elfPath, addrs)
		return decodeMsg{ref: ref, lines: lines, err: err}
	}
}

func (m *model) startLog(path string) {
	m.stopLog(false)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		m.addApp("could not open log file: " + err.Error())
		return
	}
	m.logFile = f
	m.logPath = path
	abs, _ := filepath.Abs(path)
	m.addApp("logging to " + abs)
}

func (m *model) stopLog(announce bool) {
	if m.logFile != nil {
		m.logFile.Close()
		m.logFile = nil
		m.logPath = ""
		if announce {
			m.addApp("logging stopped")
		}
	}
}

// ------------------------------------------------------------ commands -----

func restOf(raw, cmdTok string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw[1:]), cmdTok))
}

func (m *model) command(raw string) tea.Cmd {
	parts := strings.Fields(raw[1:])
	if len(parts) == 0 {
		return nil
	}
	cmd, args := strings.ToLower(parts[0]), parts[1:]
	rest := restOf(raw, parts[0])

	switch cmd {
	case "q", "quit", "exit":
		m.stopLog(false)
		return tea.Quit
	case "h", "help", "?":
		m.showHelp, m.helpOff = true, 0
	case "r", "reset":
		return resetCmd(m.link, false)
	case "b", "boot", "bootloader":
		return resetCmd(m.link, true)
	case "baud":
		n, err := strconv.Atoi(rest)
		if err != nil || n <= 0 {
			m.addApp("usage: /baud <rate>")
			break
		}
		if err := m.link.setBaud(n); err != nil {
			m.addApp("could not set baud: " + err.Error())
		} else {
			m.addApp(fmt.Sprintf("baud rate set to %d", n))
		}
	case "ts", "time", "timestamps":
		m.toggleTimestamps()
	case "pause":
		m.togglePause()
	case "sidebar", "sb":
		m.sidebarOn = !m.sidebarOn
		m.layout()
	case "theme":
		if t, ok := themes[strings.ToLower(rest)]; ok {
			m.th, m.st = t, newStyles(t)
			m.applyInputStyle()
			m.rebuild()
			m.addApp("theme: " + t.Name)
		} else {
			m.addApp("themes: " + strings.Join(themeOrder, ", "))
		}
	case "log":
		if rest == "" || strings.EqualFold(rest, "off") {
			m.stopLog(true)
		} else {
			m.startLog(rest)
		}
	case "eol":
		if v, ok := eols[strings.ToLower(rest)]; ok {
			m.eol, m.eolName = v, strings.ToLower(rest)
			m.addApp("line ending: " + m.eolName)
		} else {
			m.addApp("usage: /eol <lf|crlf|cr|none>")
		}
	case "hex":
		var data []byte
		for _, a := range args {
			v, err := strconv.ParseUint(strings.TrimPrefix(a, "0x"), 16, 8)
			if err != nil {
				m.addApp("usage: /hex 03 0d 0a")
				return nil
			}
			data = append(data, byte(v))
		}
		if err := m.link.write(data); err != nil {
			m.addApp("not connected - not sent")
		} else {
			m.addApp(fmt.Sprintf("sent %d byte(s)", len(data)))
		}
	case "ports":
		for _, p := range listPorts() {
			m.addApp(p.String())
		}
	case "clear":
		m.lines, m.rendered, m.midline = nil, nil, false
		m.forceRefresh()

	case "filter", "f":
		f, err := parseFilter(rest)
		if err != nil {
			m.addApp("bad regex: " + err.Error())
			break
		}
		m.setFilter(f)
		if f.re == nil {
			m.addApp("filter cleared")
		} else {
			mode := "showing only"
			if f.invert {
				mode = "hiding"
			}
			m.addApp(fmt.Sprintf("%s lines matching /%s/", mode, f.src))
		}

	case "hl", "highlight":
		switch {
		case rest == "":
			if len(m.hls) == 0 {
				m.addApp("no highlights - usage: /hl <regex> [color]   colors: red yellow green cyan blue mauve pink orange teal")
			}
			for i, h := range m.hls {
				m.addApp(fmt.Sprintf("%d  /%s/  (%s)", i+1, h.src, h.color))
			}
		case strings.EqualFold(rest, "clear"):
			m.hls = nil
			m.rebuild()
			m.addApp("highlights cleared")
		case len(args) == 2 && args[0] == "rm":
			n, err := strconv.Atoi(args[1])
			if err != nil || n < 1 || n > len(m.hls) {
				m.addApp("usage: /hl rm <n>")
				break
			}
			m.hls = slices.Delete(m.hls, n-1, n)
			m.rebuild()
		default:
			color := ""
			if len(args) > 1 {
				if _, ok := m.th.color(args[len(args)-1]); ok {
					color = args[len(args)-1]
					rest = strings.TrimSpace(strings.TrimSuffix(rest, color))
				}
			}
			if err := m.addHighlight(rest, color); err != nil {
				m.addApp("bad regex: " + err.Error())
			}
		}

	case "elf":
		switch {
		case rest == "":
			if m.elf == "" {
				m.addApp("no ELF set - usage: /elf <path/to/firmware.elf>")
			} else {
				m.addApp("ELF: " + m.elf)
			}
		case strings.EqualFold(rest, "off"):
			m.elf, m.tool = "", ""
			m.addApp("backtrace decoding off")
		default:
			m.setELF(rest)
		}
	case "addr2line", "tool":
		m.toolOverride = rest
		m.refreshTool()
		if rest == "" {
			m.addApp("addr2line override cleared (auto-detect)")
		} else {
			m.addApp("addr2line: " + rest)
		}
	case "decode":
		var addrs []string
		for _, a := range args {
			if hexRe.MatchString(a) {
				addrs = append(addrs, a)
			}
		}
		switch {
		case m.elf == "":
			m.addApp("set an ELF first: /elf <path>")
		case len(addrs) == 0:
			m.addApp("usage: /decode 0x400d1234 0x400d5678 ...")
		default:
			return m.decodeCmd(nil, addrs)
		}

	default:
		m.addApp(fmt.Sprintf("unknown command '/%s' - try /help", cmd))
	}
	return nil
}

func (m *model) submit() tea.Cmd {
	text := m.input.Value()
	m.input.SetValue("")
	if strings.TrimSpace(text) != "" {
		m.hist = append(m.hist, text)
	}
	m.histIdx = len(m.hist)

	var cmd tea.Cmd
	if strings.HasPrefix(text, "/") && !strings.HasPrefix(text, "//") {
		cmd = m.command(text)
	} else {
		text = strings.TrimPrefix(text, "/") // "//foo" -> "/foo"
		m.midline = false
		if err := m.link.write([]byte(text + m.eol)); err != nil {
			m.addApp("not connected - message not sent")
		} else {
			m.addTx(text)
		}
	}
	if m.paused {
		m.togglePause()
	} else {
		m.refresh()
		m.vp.GotoBottom()
	}
	return cmd
}

// ------------------------------------------------------------ update -------

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		return m, nil

	case tickMsg:
		rx := float64(m.link.rx.Load())
		m.rxRate = rx - m.rxPrev
		m.rxPrev = rx
		return m, tick()

	case devBatch:
		var cmds []tea.Cmd
		for _, ev := range msg {
			l := m.addDev(ev.text, ev.partial)
			if !ev.partial && m.elf != "" {
				if addrs := extractAddrs(l.text); len(addrs) > 0 {
					cmds = append(cmds, m.decodeCmd(l, addrs))
				}
			}
		}
		m.refresh()
		return m, tea.Batch(cmds...)

	case decodeMsg:
		if msg.err != nil {
			m.addApp("decode failed: " + msg.err.Error())
			m.refresh()
			return m, nil
		}
		var ls []*line
		for _, t := range msg.lines {
			ls = append(ls, &line{kind: kindDecode, text: t, visible: true})
			m.logWrite("    ↳ " + t + "\n")
		}
		if len(ls) == 0 {
			m.addApp("decode: nothing resolved")
		} else if msg.ref != nil {
			m.insertAfter(msg.ref, ls)
		} else {
			for _, l := range ls {
				m.push(l)
			}
		}
		m.refresh()
		return m, nil

	case linkMsg:
		m.connected = msg.connected
		if msg.connected {
			m.connectedAt = time.Now()
		}
		m.addApp(msg.text)
		m.refresh()
		return m, nil

	case appMsg:
		m.addApp(msg.text)
		m.refresh()
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m *model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	if key == "ctrl+c" {
		m.stopLog(false)
		return m, tea.Quit
	}

	if m.showHelp {
		switch key {
		case "esc", "f1", "q", "enter", "?":
			m.showHelp = false
		case "up", "k":
			m.helpOff = max(m.helpOff-1, 0)
		case "down", "j":
			m.helpOff++
		case "pgup":
			m.helpOff = max(m.helpOff-10, 0)
		case "pgdown":
			m.helpOff += 10
		}
		return m, nil
	}

	switch key {
	case "f1":
		m.showHelp, m.helpOff = true, 0
		return m, nil
	case "f2":
		m.sidebarOn = !m.sidebarOn
		m.layout()
		return m, nil
	case "f3":
		m.toggleTimestamps()
		return m, nil
	case "f4":
		m.togglePause()
		return m, nil
	case "f5":
		return m, resetCmd(m.link, false)
	case "f6":
		return m, resetCmd(m.link, true)
	case "tab":
		if m.focus == focusInput {
			m.setFocus(focusOutput)
		} else {
			m.setFocus(focusInput)
		}
		return m, nil
	}

	// ---- output panel focused: vim-ish scrolling
	if m.focus == focusOutput {
		var cmd tea.Cmd
		switch key {
		case "esc", "enter", "i":
			m.setFocus(focusInput)
		case "g", "home":
			m.vp.GotoTop()
		case "G", "end":
			m.vp.GotoBottom()
		case "p":
			m.togglePause()
		case "c":
			m.lines, m.rendered, m.midline = nil, nil, false
			m.forceRefresh()
		case "?":
			m.showHelp, m.helpOff = true, 0
		default:
			m.vp, cmd = m.vp.Update(msg)
		}
		return m, cmd
	}

	// ---- input panel focused
	switch key {
	case "ctrl+d":
		if m.input.Value() == "" {
			m.stopLog(false)
			return m, tea.Quit
		}
	case "enter":
		return m, m.submit()
	case "pgup", "pgdown":
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	case "esc":
		if m.paused {
			m.togglePause()
		} else {
			m.vp.GotoBottom()
		}
		return m, nil
	case "up":
		if m.histIdx > 0 {
			m.histIdx--
			m.input.SetValue(m.hist[m.histIdx])
			m.input.CursorEnd()
		}
		return m, nil
	case "down":
		if m.histIdx < len(m.hist)-1 {
			m.histIdx++
			m.input.SetValue(m.hist[m.histIdx])
		} else {
			m.histIdx = len(m.hist)
			m.input.SetValue("")
		}
		m.input.CursorEnd()
		return m, nil
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}
