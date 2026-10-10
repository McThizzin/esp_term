package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"go.bug.st/serial"
	"go.bug.st/serial/enumerator"
)

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

// ------------------------------------------------------------- link --------

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

	rx, tx atomic.Int64
}

func (l *Link) get() serial.Port {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.port
}

func (l *Link) getBaud() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.baud
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
	n, err := p.Write(b)
	l.tx.Add(int64(n))
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

func resetCmd(l *Link, bootloader bool) tea.Cmd {
	return func() tea.Msg {
		if err := l.reset(bootloader); err != nil {
			return appMsg{"reset failed: " + err.Error()}
		}
		if bootloader {
			return appMsg{"entered download mode (if your board supports auto-reset)"}
		}
		return appMsg{"reset"}
	}
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
			msg := fmt.Sprintf("connected to %s @ %d baud", l.name, l.getBaud())
			if first {
				msg += "   (F1 or /help for help)"
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
			l.rx.Add(int64(n))
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
