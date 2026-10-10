## esp_term ##

### Go ###

**Build (Go 1.21+)**
```bash
mkdir esp_term && cd esp_term
go mod init esp_term
go mod tidy                        # fetches bubbletea, bubbles, lipgloss, go.bug.st/serial
go build -ldflags "-s -w" -o build/esp_term
```

**Cross-compiling**
```bash
GOOS=windows GOARCH=amd64 go build -ldflags "-s -w" -o build/esp_term.exe
GOOS=linux   GOARCH=arm64 go build -ldflags "-s -w" -o build/esp_term-arm64
```
Build the macOS binary on a Mac. The serial library's port enumerator uses IOKit there, so cross-compiling from Linux or Windows won't work.

**Files**
- `main.go`: flags and startup
- `model.go`: state, update loop and slash commands
- `view.go`: panels, sidebar, footer and help overlay
- `theme.go`: palettes, styles and icons
- `link.go`: serial port handling and discovery
- `decode.go`: ELF and `addr2line` handling

**Features**
- **Pinned prompt:** Output scrolls in the top pane, a status bar sits in the middle, and your input line stays on the bottom row. Incoming logs no longer interleave with what you're typing.
- **Status bar:** It shows the port, baud, connection state, timestamp setting, line ending and log file. It also tells you when you've scrolled back.
- **Scrolling and history:** PgUp/PgDn scroll the output, Esc jumps back to the bottom, and Up/Down recall command history. New output doesn't yank you down while you're reading older lines.
- **Typing aids:** The input supports cursor movement, paste, and word-wrapped long lines. Text you send is echoed in blue with a `»` prefix, since the board doesn't echo it back.
- It uses the alternate screen, like `vim` or `htop`, so output doesn't stay in your normal terminal scrollback after you quit. PgUp/PgDn covers scrolling during a session, and `-l file.log` keeps a permanent copy.
- The device's own ANSI codes are stripped and replaced with the app's coloring. This keeps stray escape sequences from corrupting the layout.
- **Titled borders:** Lipgloss can't put text in a border, so `panel()` in `view.go` draws the rounded border by hand with the title and a status chip embedded in the top line.
- **Panels:** The sidebar on the left has Device, Filter, Decoder and Highlights panels. Serial Output is in the middle, and Input is its own bordered panel at the bottom. The sidebar hides itself below 90 columns, or toggle it with F2.
- **Focus:** Tab moves focus between the input and the output panel, and the focused border turns accent-colored. With output focused you get vim-style keys: `j/k`, `g/G`, `p` to pause and `c` to clear.
- **Footer and help:** A hint bar at the bottom changes with the focused panel. F1 or `?` opens a centered help overlay.
- **Themes:** `-theme mocha|tokyo|gruvbox|nord`, or `/theme name` to switch live. Catppuccin Mocha is the default. Adding a theme is one map entry in `theme.go`.
- **Icons:** `-nerd` adds Nerd Font glyphs to the panel titles. Without it the titles stay plain.
- **Filter:** `/filter wifi|mqtt` shows only matching lines. `/filter -v noisy_tag` hides matches, and `/filter off` clears it. The sidebar shows `shown/total`, and `-filter` sets one at startup.
- **Highlights:** `/hl error red`, `/hl "my_tag" teal`, `/hl rm 2` and `/hl clear`. They render as colored badges inside lines, `-hl` is repeatable, and regexes are case-insensitive unless you add `(?-i)`.
- **Backtrace decoding:** When an ELF is set, `Backtrace:` lines and `PC`/`MEPC` register lines are decoded with `addr2line`. The result appears as `↳ func at file.c:42` lines inserted right under the crash line.
  - The ELF is auto-detected from `build/*.elf` (ESP-IDF) or `.pio/build/*/firmware.elf` (PlatformIO). Override it with `-elf` or `/elf path`.
  - It reads the ELF header to pick Xtensa or RISC-V, then finds the matching `addr2line` on your `PATH` or in `~/.espressif/tools`. Override that with `-addr2line` or `/addr2line path`.
  - `/decode 0x400d1234 ...` decodes addresses by hand.
- **Pause:** F4 freezes the view while data keeps buffering, and the title shows `⏸ paused +N`.

Everything else works the same as the Python version: the same flags (`-p -b -t -l -eol -reset -list`) and the same `/` commands. The log-level coloring, panic highlighting, auto-reconnect and reset/bootloader sequences carry over too.

**Caveats**
- F1 is sometimes grabbed by the terminal emulator (GNOME Terminal opens its own help). Every F-key has a slash-command equivalent.
- The decoder keys off ESP-IDF's usual crash output. If your firmware prints backtraces differently adjust the regexes.

---

### Python ###
A single-file Python app that does the core of what web terminals do, plus a few things the browser ones can't. `esp_term.py`, is a single-file serial terminal for ESP32 that needs only `pyserial`.

**Setup and run**
```bash
pip install pyserial
python esp_term.py                       # auto-detects the port, 115200 baud
python esp_term.py -p /dev/ttyUSB0 -b 921600 -t -l session.log
python esp_term.py --list                # just list ports
```

**What it does**
- **Port detection:** It prefers Espressif native USB, CP210x, CH340/CH9102 and FTDI devices, and shows a picker if more than one is found.
- **Log coloring:** ESP-IDF log lines are colored by level (E red, W yellow, I green, D cyan, V grey). Panics, `Backtrace:`, watchdog and brownout lines are highlighted in bold red.
- **Timestamps and logging:** `-t` adds millisecond timestamps. `-l file` writes a plain-text log with the color codes stripped.
- **Reset and bootloader:** `/reset` pulses EN via RTS, and `/boot` runs the DTR/RTS sequence that esptool uses to enter download mode.
- **Auto-reconnect:** If the device disappears, for example when an S2/S3/C3 native USB chip re-enumerates after a reset, it waits and reconnects on its own.
- **No reset on connect:** By default it holds DTR/RTS low so opening the port doesn't reboot your board. Pass `--reset` if you want the reboot, to catch boot logs.
- **Prompts without newlines:** Text like `> ` appears right away instead of waiting for a newline.

Anything you type is sent to the board with a line ending, `lf` by default (change it with `--eol` or `/eol`). Lines starting with `/` are commands:

| Command | Action |
|---|---|
| `/reset`, `/boot` | Reset the chip, or reset into download mode |
| `/baud 921600` | Change baud rate live |
| `/ts` | Toggle timestamps |
| `/log file.txt`, `/log off` | Start or stop logging |
| `/eol crlf` | Change the line ending |
| `/hex 03 0d` | Send raw bytes |
| `/ports`, `/clear`, `/help`, `/quit` | Utility commands |

To send text that starts with a slash, use `//`.

**Notes**
- On Linux you'll probably need `sudo usermod -aG dialout $USER`, then log out and back in.
- Incoming output can interleave with text you're mid-typing, since it's a plain line-based input.
- `/boot` only works on boards with the standard auto-reset circuit. Native-USB chips may need the BOOT button.

