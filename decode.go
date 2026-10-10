package main

import (
	"context"
	"debug/elf"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

var (
	btPairRe = regexp.MustCompile(`0x[0-9a-fA-F]{8}:0x[0-9a-fA-F]{8}`)
	regPCRe  = regexp.MustCompile(`\b(?:M?EPC|PC)\s*:\s*(0x[0-9a-fA-F]{8})`)
	hexRe    = regexp.MustCompile(`^0x[0-9a-fA-F]+$`)
)

// extractAddrs pulls code addresses out of ESP-IDF / Arduino-ESP32 crash output:
//
//	Backtrace: 0x400d1234:0x3ffb1e50 0x400d5678:0x3ffb1e70 ...
//	PC      : 0x400d1234  PS      : 0x00060030 ...      (Xtensa)
//	MEPC    : 0x42001234  RA      : 0x42001aa6 ...      (RISC-V)
func extractAddrs(text string) []string {
	if strings.Contains(text, "Backtrace:") {
		var out []string
		for _, p := range btPairRe.FindAllString(text, -1) {
			out = append(out, p[:10]) // the PC half of pc:sp
		}
		return out
	}
	if m := regPCRe.FindStringSubmatch(text); m != nil {
		return []string{m[1]}
	}
	return nil
}

// autoDetectELF looks for the usual build output locations in the current
// directory (ESP-IDF: build/*.elf, PlatformIO: .pio/build/*/firmware.elf)
// and returns the most recently modified one.
func autoDetectELF() string {
	var best string
	var bestTime time.Time
	for _, pat := range []string{"build/*.elf", ".pio/build/*/firmware.elf"} {
		matches, _ := filepath.Glob(pat)
		for _, p := range matches {
			if st, err := os.Stat(p); err == nil && st.ModTime().After(bestTime) {
				best, bestTime = p, st.ModTime()
			}
		}
	}
	return best
}

// findTool locates a suitable addr2line for the given ELF. It checks the ELF
// machine type (Xtensa vs RISC-V), then PATH, then the ESP-IDF tools dir.
func findTool(elfPath string) (string, error) {
	var names []string
	if f, err := elf.Open(elfPath); err == nil {
		switch f.Machine {
		case elf.EM_XTENSA:
			names = append(names,
				"xtensa-esp-elf-addr2line", "xtensa-esp32-elf-addr2line",
				"xtensa-esp32s3-elf-addr2line", "xtensa-esp32s2-elf-addr2line")
		case elf.EM_RISCV:
			names = append(names, "riscv32-esp-elf-addr2line")
		}
		f.Close()
	}
	names = append(names,
		"riscv32-esp-elf-addr2line", "xtensa-esp-elf-addr2line",
		"xtensa-esp32-elf-addr2line", "xtensa-esp32s3-elf-addr2line",
		"xtensa-esp32s2-elf-addr2line", "llvm-addr2line", "addr2line")

	for _, n := range names {
		if p, err := exec.LookPath(n); err == nil {
			return p, nil
		}
	}

	base := os.Getenv("IDF_TOOLS_PATH")
	if base == "" {
		if home, err := os.UserHomeDir(); err == nil {
			base = filepath.Join(home, ".espressif")
		}
	}
	if base != "" {
		for _, n := range names {
			if runtime.GOOS == "windows" {
				n += ".exe"
			}
			// tools/<toolchain>/<version>/<toolchain>/bin/<name>
			matches, _ := filepath.Glob(filepath.Join(base, "tools", "*", "*", "*", "bin", n))
			if len(matches) > 0 {
				sort.Strings(matches)
				return matches[len(matches)-1], nil
			}
		}
	}
	return "", errors.New("no addr2line found (install the ESP toolchain, or use /addr2line <path>)")
}

func runAddr2line(tool, elfPath string, addrs []string) ([]string, error) {
	if _, err := os.Stat(elfPath); err != nil {
		return nil, err
	}
	if tool == "" {
		var err error
		if tool, err = findTool(elfPath); err != nil {
			return nil, err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	args := append([]string{"-pfiaC", "-e", elfPath}, addrs...)
	out, err := exec.CommandContext(ctx, tool, args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s: %v: %s", filepath.Base(tool), err, strings.TrimSpace(string(out)))
	}
	var res []string
	for _, l := range strings.Split(string(out), "\n") {
		l = strings.TrimRight(l, "\r ")
		if strings.TrimSpace(l) == "" || strings.Contains(l, ": ?? ") {
			continue
		}
		res = append(res, l)
	}
	return res, nil
}
