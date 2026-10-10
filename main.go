// esp_term - serial terminal for ESP32 debugging.
//
// Build:
//   go mod init esp_term && go mod tidy
//   go build -ldflags "-s -w" -o esp_term .
package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"go.bug.st/serial"
	"go.bug.st/serial/enumerator"
)

const maxLines = 20000

var prog *tea.Program

// ------------------------------------------------------------- styles ------

var (
	dimStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	appStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("5"))
	txStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("12"))
	alertStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("9"))
	plainStyle = lipgloss.NewStyle()
	levelStyle = map[byte]lipgloss.Style{
		'E': lipgloss.NewStyle().Foreground(lipgloss.Color("1")),
		'W': lipgloss.NewStyle().Foreground(lipgloss.Color("3")),
		'I': lipgloss.NewStyle().Foreground(lipgloss.Color("2")),
		'D': lipgloss.NewStyle().Foreground(lipgloss.Color("6")),
		'V': lipgloss.NewStyle().Foreground(lipgloss.Color("8")),
	}
	statusStyle = lipgloss.NewStyle().Reverse(true)

	ansiRe  = regexp.MustCompile(`\x1b(\[[0-?]*[ -/]*[@-~]|[@-Z\\-_])`)
	logRe   = regexp.MustCompile(`^([EWIDV]) \(\d+\) `)
	alertRe = regexp.MustCompile(`(?i)Guru Meditation|abort\(\)|Backtrace:|assert failed|Task watchdog|Brownout|rst:0x[0-9a-f]+ \((TG\dWDT|RTCWDT|SW_CPU|WDT)|panic'ed`)
)

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

func classify(text string) lipgloss.Style {
	if alertRe.MatchString(text) {
		return alertStyle
	}
	if m := logRe.FindStringSubmatch(text); m != nil {
		return levelStyle[m[1][0]]
	}
	return plainStyle
}

// ------------------------------------------------------------- ports -------

var knownVIDs = map[string]string{
	"303A": "Espressif native USB",
	"10C4": "CP210x",
	"1A86": "CH340/CH9102",
	"0403": "FTDI",
}

type portInfo struct {
	Name, Desc, VID, PID string
	USB, Known           bool
}

func (p portInfo) String() string {
	s := fmt.Sprintf("%-22s", p.Name)
	if p.VID != "" {
		s += fmt.Sprintf(" %s:%s", p.VID, p.PID)
	}
	if p.Desc != "" {
		s += "  " + p.Desc
	}
	if tag, ok := knownVIDs[p.VID]; ok {
		s += "  [" + tag + "]"
	}
	return s
}

func listPorts() []portInfo {
	var out []portInfo
	details, err := enumerator.GetDetailedPortsList()
	if err == nil {
		for _, d := range details {
			if runtime.GOOS == "darwin" && strings.HasPrefix(d.Name, "/dev/tty.") {
				continue // prefer the cu.* twin
			}
			vid := strings.ToUpper(d.VID)
			_, known := knownVIDs[vid]
			out = append(out, portInfo{d.Name, d.Product, vid, strings.ToUpper(d.PID), d.IsUSB, known})
		}
	}
	if len(out) == 0 {
		names, _ := serial.GetPortsList()
		for _, n := range names {
			out = append(out, portInfo{Name: n})
		}
	}
	// if any USB ports exist, hide legacy non-USB ones (ttyS0...)
	hasUSB := false
	for _, p := range out {
		hasUSB = hasUSB || p.USB
	}
	if hasUSB {
		filtered := out[:0]
		for _, p := range out {
			if p.USB {
				filtered = append(filtered, p)
			}
		}
		out = filtered
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Known != out[j].Known {
			return out[i].Known
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func choosePort() string {
	ports := listPorts()
	if len(ports) == 0 {
		fmt.Fprintln(os.Stderr, "No serial ports found. Is the board plugged in?")
		os.Exit(1)
	}
	var known []portInfo
	for _, p := range ports {
		if p.Known {
			known = append(known, p)
		}
	}
	if len(known) == 1 {
		return known[0].Name
	}
	if len(ports) == 1 {
		return ports[0].Name
	}
	fmt.Println("Multiple serial ports found:")
	for i, p := range ports {
		fmt.Printf("  [%d] %s\n", i, p)
	}
	in := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("Select port number: ")
		line, err := in.ReadString('\n')
		if err != nil {
			os.Exit(0)
		}
		n, err := strconv.Atoi(strings.TrimSpace(line))
		if err == nil && n >= 0 && n < len(ports) {
			return ports[n].Name
		}
		fmt.Println("Invalid choice.")
	}
}

// ------------------------------------------------------------- serial ------

type devEvent struct {
	text    string
	partial bool
}
type devBatch []devEvent
type linkMsg struct {
	connected bool
	text      string
}
type appMsg struct{ text string }

var errNotConnected = errors.New("not connected")

type Link struct {
	mu   sync.Mutex
	port serial.Port
	name string
	baud int
	hold bool
}

func (l *Link) get() serial.Port {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.port
}

func (l *Link) open() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	p, err := serial.Open(l.name, &serial.Mode{BaudRate: l.baud})
	if err != nil {
		return err
	}
	_ = p.SetReadTimeout(50 * time.Millisecond)
	if l.hold {
		// keep EN / GPIO0 released so opening the port doesn't reset the board
		_ = p.SetDTR(false)
		_ = p.SetRTS(false)
	}
	l.port = p
	return nil
}

func (l *Link) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.port != nil {
		_ = l.port.Close()
		l.port = nil
	}
}

func (l *Link) write(b []byte) error {
	p := l.get()
	if p == nil {
		return errNotConnected
	}
	_, err := p.Write(b)
	return err
}

func (l *Link) setBaud(n int) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.baud = n
	if l.port != nil {
		return l.port.SetMode(&serial.Mode{BaudRate: n})
	}
	return nil
}

func (l *Link) reset(bootloader bool) error {
	p := l.get()
	if p == nil {
		return errNotConnected
	}
	if bootloader {
		// DTR -> GPIO0, RTS -> EN (standard esptool auto-download sequence)
		_ = p.SetDTR(false)
		_ = p.SetRTS(true)
		time.Sleep(100 * time.Millisecond)
		_ = p.SetDTR(true)
		_ = p.SetRTS(false)
		time.Sleep(50 * time.Millisecond)
		return p.SetDTR(false)
	}
	_ = p.SetDTR(false)
	if err := p.SetRTS(true); err != nil {
		return err
	}
	time.Sleep(100 * time.Millisecond)
	return p.SetRTS(false)
}

func send(m tea.Msg) {
	if prog != nil {
		prog.Send(m)
	}
}

func (l *Link) readLoop() {
	buf := make([]byte, 4096)
	var pending []byte
	lastRx := time.Now()
	lastProbe := time.Now()
	probeOK := false
	warned := false
	first := true

	for {
		p := l.get()
		if p == nil {
			if err := l.open(); err != nil {
				if !warned {
					send(linkMsg{false, "waiting for " + l.name + " ..."})
					warned = true
				}
				time.Sleep(500 * time.Millisecond)
				continue
			}
			warned = false
			pending = nil
			p = l.get()
			_, perr := p.GetModemStatusBits()
			probeOK = perr == nil
			lastRx, lastProbe = time.Now(), time.Now()
			msg := fmt.Sprintf("connected to %s @ %d baud", l.name, l.baud)
			if first {
				msg += "   (type /help for commands)"
				first = false
			}
			send(linkMsg{true, msg})
		}

		n, err := p.Read(buf)
		if err != nil {
			l.disconnect(&pending)
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if n > 0 {
			lastRx = time.Now()
			pending = append(pending, buf[:n]...)
			var batch devBatch
			for {
				i := bytes.IndexByte(pending, '\n')
				if i < 0 {
					break
				}
				batch = append(batch, devEvent{string(pending[:i]), false})
				pending = pending[i+1:]
			}
			if len(batch) > 0 {
				send(batch)
			}
			continue
		}
		// idle
		if len(pending) > 0 && time.Since(lastRx) > 200*time.Millisecond {
			send(devBatch{{string(pending), true}}) // prompt without newline
			pending = nil
		}
		if probeOK && time.Since(lastProbe) > time.Second {
			lastProbe = time.Now()
			if _, err := p.GetModemStatusBits(); err != nil {
				l.disconnect(&pending)
				time.Sleep(500 * time.Millisecond)
			}
		}
	}
}

func (l *Link) disconnect(pending *[]byte) {
	if len(*pending) > 0 {
		send(devBatch{{string(*pending), false}})
		*pending = nil
	}
	l.close()
	send(linkMsg{false, "device disconnected"})
}

// ------------------------------------------------------------- model -------

const (
	kindDev = iota
	kindApp
	kindTx
)

type line struct {
	kind  int
	stamp string
	text  string
}

type model struct {
	link  *Link
	input textinput.Model
	vp    viewport.Model
	ready bool

	width, height int
	lines         []*line
	rendered      []string
	midline       bool
	connected     bool
	timestamps    bool
	eol           string
	eolName       string

	hist    []string
	histIdx int

	logFile *os.File
	logPath string
}

func newModel(link *Link, ts bool, eolName string) *model {
	ti := textinput.New()
	ti.Prompt = "> "
	ti.Focus()
	ti.CharLimit = 0
	return &model{link: link, input: ti, timestamps: ts, eol: eols[eolName], eolName: eolName}
}

var eols = map[string]string{"lf": "\n", "crlf": "\r\n", "cr": "\r", "none": ""}

func (m *model) Init() tea.Cmd { return textinput.Blink }

func (m *model) renderLine(l *line) string {
	var body string
	switch l.kind {
	case kindApp:
		body = appStyle.Render("[esp_term] " + l.text)
	case kindTx:
		body = txStyle.Render("» " + l.text)
	default:
		body = classify(l.text).Render(l.text)
	}
	if m.timestamps && l.stamp != "" {
		body = dimStyle.Render(l.stamp) + " " + body
	}
	if m.width > 0 {
		return lipgloss.NewStyle().Width(m.width).Render(body)
	}
	return body
}

func (m *model) logWrite(s string) {
	if m.logFile != nil {
		_, _ = m.logFile.WriteString(s)
	}
}

func stampNow() string { return time.Now().Format("15:04:05.000") }

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
	m.push(&line{kind: kindApp, stamp: stampNow(), text: text})
	m.logWrite("[esp_term] " + text + "\n")
}

func (m *model) addTx(text string) {
	m.midline = false
	m.push(&line{kind: kindTx, stamp: stampNow(), text: text})
	m.logWrite("» " + text + "\n")
}

func (m *model) addDev(text string, partial bool) {
	text = clean(text)
	if m.midline && len(m.lines) > 0 && m.lines[len(m.lines)-1].kind == kindDev {
		last := m.lines[len(m.lines)-1]
		last.text += text
		m.rendered[len(m.rendered)-1] = m.renderLine(last)
		m.logWrite(text)
	} else {
		st := stampNow()
		m.push(&line{kind: kindDev, stamp: st, text: text})
		m.logWrite(st + " " + text)
	}
	if !partial {
		m.logWrite("\n")
	}
	m.midline = partial
}

func (m *model) refresh() {
	if !m.ready {
		return
	}
	atBottom := m.vp.AtBottom()
	m.vp.SetContent(strings.Join(m.rendered, "\n"))
	if atBottom {
		m.vp.GotoBottom()
	}
}

func (m *model) rebuild() {
	for i, l := range m.lines {
		m.rendered[i] = m.renderLine(l)
	}
	m.refresh()
}

func (m *model) startLog(path string) {
	m.stopLog(false)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		m.addApp("could not open log file: " + err.Error())
		return
	}
	m.logFile = f
	abs, _ := filepath.Abs(path)
	m.logPath = path
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

const helpText = `commands (anything else you type is sent to the device):
  /reset             reset the chip (RTS -> EN)
  /boot              reset into download mode (DTR/RTS)
  /baud <rate>       change baud rate
  /ts                toggle timestamps
  /log <file|off>    start/stop logging to a file
  /eol <lf|crlf|cr|none>   line ending appended to sent text
  /hex <bytes>       send raw bytes, e.g. /hex 03 0d 0a
  /ports             list serial ports
  /clear             clear the screen
  /quit              exit (Ctrl+C / Ctrl+D also work)
keys: Up/Down = history, PgUp/PgDn = scroll, Esc = jump to bottom
use // to send text that starts with a slash`

func (m *model) command(raw string) tea.Cmd {
	parts := strings.Fields(raw[1:])
	if len(parts) == 0 {
		return nil
	}
	cmd, args := strings.ToLower(parts[0]), parts[1:]
	link := m.link

	switch cmd {
	case "q", "quit", "exit":
		return tea.Quit
	case "h", "help", "?":
		for _, l := range strings.Split(helpText, "\n") {
			m.addApp(l)
		}
	case "r", "reset":
		return func() tea.Msg {
			if err := link.reset(false); err != nil {
				return appMsg{"reset failed: " + err.Error()}
			}
			return appMsg{"reset"}
		}
	case "b", "boot", "bootloader":
		return func() tea.Msg {
			if err := link.reset(true); err != nil {
				return appMsg{"boot reset failed: " + err.Error()}
			}
			return appMsg{"entered download mode (if your board supports auto-reset)"}
		}
	case "baud":
		if len(args) != 1 {
			m.addApp("usage: /baud <rate>")
			break
		}
		n, err := strconv.Atoi(args[0])
		if err != nil || n <= 0 {
			m.addApp("usage: /baud <rate>")
			break
		}
		if err := link.setBaud(n); err != nil {
			m.addApp("could not set baud: " + err.Error())
		} else {
			m.addApp(fmt.Sprintf("baud rate set to %d", n))
		}
	case "ts", "time", "timestamps":
		m.timestamps = !m.timestamps
		m.rebuild()
		m.addApp("timestamps " + map[bool]string{true: "on", false: "off"}[m.timestamps])
	case "log":
		if len(args) == 0 || strings.ToLower(args[0]) == "off" {
			m.stopLog(true)
		} else {
			m.startLog(strings.Join(args, " "))
		}
	case "eol":
		if len(args) == 1 {
			if v, ok := eols[strings.ToLower(args[0])]; ok {
				m.eol, m.eolName = v, strings.ToLower(args[0])
				m.addApp("line ending: " + m.eolName)
				break
			}
		}
		m.addApp("usage: /eol <lf|crlf|cr|none>")
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
		if err := link.write(data); err != nil {
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
	m.refresh()
	m.vp.GotoBottom()
	return cmd
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		vh := max(msg.Height-2, 1)
		if !m.ready {
			m.vp = viewport.New(msg.Width, vh)
			m.ready = true
		} else {
			m.vp.Width, m.vp.Height = msg.Width, vh
		}
		m.input.Width = max(msg.Width-lipgloss.Width(m.input.Prompt)-1, 5)
		m.rebuild()
		return m, nil

	case devBatch:
		for _, ev := range msg {
			m.addDev(ev.text, ev.partial)
		}
		m.refresh()
		return m, nil

	case linkMsg:
		m.connected = msg.connected
		m.addApp(msg.text)
		m.refresh()
		return m, nil

	case appMsg:
		m.addApp(msg.text)
		m.refresh()
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "ctrl+d":
			m.stopLog(false)
			return m, tea.Quit
		case "enter":
			cmd := m.submit()
			return m, cmd
		case "pgup", "pgdown":
			var cmd tea.Cmd
			m.vp, cmd = m.vp.Update(msg)
			return m, cmd
		case "esc":
			m.vp.GotoBottom()
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
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m *model) statusBar() string {
	state := "○ disconnected"
	if m.connected {
		state = "● connected"
	}
	ts := "off"
	if m.timestamps {
		ts = "on"
	}
	s := fmt.Sprintf(" %s @ %d  %s  │ ts:%s  eol:%s", m.link.name, m.link.baud, state, ts, m.eolName)
	if m.logPath != "" {
		s += "  log:" + filepath.Base(m.logPath)
	}
	if !m.vp.AtBottom() {
		s += "  │ ▼ scrolled back (Esc = bottom)"
	} else {
		s += "  │ PgUp/PgDn scroll · /help"
	}
	return statusStyle.Width(m.width).MaxWidth(m.width).Render(s)
}

func (m *model) View() string {
	if !m.ready {
		return "starting..."
	}
	return m.vp.View() + "\n" + m.statusBar() + "\n" + m.input.View()
}

// ------------------------------------------------------------- main --------

func main() {
	portFlag := flag.String("p", "", "serial port (default: auto-detect)")
	baud := flag.Int("b", 115200, "baud rate")
	ts := flag.Bool("t", false, "show timestamps")
	logPath := flag.String("l", "", "also log output to this file")
	eol := flag.String("eol", "lf", "line ending for sent text: lf|crlf|cr|none")
	allowReset := flag.Bool("reset", false, "allow the board to reset when the port opens")
	list := flag.Bool("list", false, "list serial ports and exit")
	flag.Parse()

	if *list {
		for _, p := range listPorts() {
			fmt.Println(p)
		}
		return
	}
	if _, ok := eols[*eol]; !ok {
		fmt.Fprintln(os.Stderr, "invalid -eol; use lf, crlf, cr or none")
		os.Exit(2)
	}

	port := *portFlag
	if port == "" {
		port = choosePort()
	}

	link := &Link{name: port, baud: *baud, hold: !*allowReset}
	m := newModel(link, *ts, *eol)
	if *logPath != "" {
		m.startLog(*logPath)
	}

	prog = tea.NewProgram(m, tea.WithAltScreen())
	go link.readLoop()
	if _, err := prog.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	link.close()
}
