// esp_term - a serial terminal for ESP32 development, built with Bubble Tea.
//
// Build:
//
//	go mod init esp_term && go mod tidy
//	go build -ldflags "-s -w" -o build/esp_term
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

var prog *tea.Program

func main() {
	portFlag := flag.String("p", "", "serial port (default: auto-detect)")
	baud := flag.Int("b", 115200, "baud rate")
	ts := flag.Bool("t", false, "show timestamps")
	logPath := flag.String("l", "", "also log output to this file")
	eol := flag.String("eol", "lf", "line ending for sent text: lf|crlf|cr|none")
	allowReset := flag.Bool("reset", false, "allow the board to reset when the port opens")
	list := flag.Bool("list", false, "list serial ports and exit")
	themeName := flag.String("theme", "mocha", "color theme: "+strings.Join(themeOrder, "|"))
	nerd := flag.Bool("nerd", false, "use Nerd Font icons in panel titles")
	filterArg := flag.String("filter", "", "only show lines matching this regex (prefix with ! to exclude)")
	elfPath := flag.String("elf", "", "firmware ELF for backtrace decoding (default: auto-detect build/*.elf)")
	tool := flag.String("addr2line", "", "path to addr2line (default: auto-detect)")
	var hls []string
	flag.Func("hl", "highlight regex (repeatable)", func(s string) error {
		hls = append(hls, s)
		return nil
	})
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
	th, ok := themes[strings.ToLower(*themeName)]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown theme %q; choose one of: %s\n", *themeName, strings.Join(themeOrder, ", "))
		os.Exit(2)
	}
	icons := asciiIcons
	if *nerd {
		icons = nerdIcons
	}

	port := *portFlag
	if port == "" {
		port = choosePort()
	}

	link := &Link{name: port, baud: *baud, hold: !*allowReset}
	m := newModel(link, th, icons, *ts, *eol)
	m.toolOverride = *tool

	if *logPath != "" {
		m.startLog(*logPath)
	}
	if *filterArg != "" {
		f, err := parseFilter(*filterArg)
		if err != nil {
			fmt.Fprintln(os.Stderr, "bad -filter regex:", err)
			os.Exit(2)
		}
		m.setFilter(f)
	}
	for _, h := range hls {
		if err := m.addHighlight(h, ""); err != nil {
			fmt.Fprintf(os.Stderr, "bad -hl regex %q: %v\n", h, err)
			os.Exit(2)
		}
	}
	switch {
	case *elfPath != "":
		m.setELF(*elfPath)
	default:
		if p := autoDetectELF(); p != "" {
			m.addApp("auto-detected ELF: " + p)
			m.setELF(p)
		}
	}

	prog = tea.NewProgram(m, tea.WithAltScreen())
	go link.readLoop()
	if _, err := prog.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	link.close()
}
