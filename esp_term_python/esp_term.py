#!/usr/bin/env python3
"""
esp_term - a small serial terminal for ESP32 development.

Features:
  * Auto-detects ESP32 / USB-serial ports (or lets you pick one)
  * ESP-IDF log level coloring (E/W/I/D/V) + highlights for panics/backtraces
  * Optional timestamps, logging to file (plain text, no ANSI)
  * Reset / bootloader entry via DTR/RTS (same as esptool / web flashers)
  * Auto-reconnect (handy for native-USB chips that re-enumerate on reset)
  * Slash commands for baud, reset, log, etc. Everything else is sent to the board.

Install:   pip install pyserial
Run:       python esp_term.py                 (auto-detect port, 115200)
           python esp_term.py -p /dev/ttyUSB0 -b 921600 -t -l session.log
"""

import argparse
import datetime
import os
import re
import sys
import threading
import time

try:
    import serial
    from serial.tools import list_ports
except ImportError:
    sys.exit("pyserial is required:  pip install pyserial")

try:
    import readline  # noqa: F401  (command history on Linux/macOS)
except ImportError:
    pass

# ---------------------------------------------------------------- colors ----
RESET = "\x1b[0m"
DIM = "\x1b[2m"
COLORS = {
    "E": "\x1b[31m",   # red
    "W": "\x1b[33m",   # yellow
    "I": "\x1b[32m",   # green
    "D": "\x1b[36m",   # cyan
    "V": "\x1b[90m",   # grey
}
ALERT = "\x1b[1;31m"   # bold red
INFO = "\x1b[35m"      # magenta, for this app's own messages

ANSI_RE = re.compile(r"\x1b\[[0-9;]*[A-Za-z]")
LOG_RE = re.compile(r"^([EWIDV]) \(\d+\) ")
ALERT_RE = re.compile(
    r"Guru Meditation|abort\(\)|Backtrace:|assert failed|Task watchdog|"
    r"Brownout|rst:0x[0-9a-f]+ \((?:TG\dWDT|RTCWDT|SW_CPU|WDT)|panic'ed",
    re.I,
)

# Common ESP32 USB-serial bridges: (VID)
KNOWN_VIDS = {
    0x303A: "Espressif native USB",
    0x10C4: "CP210x",
    0x1A86: "CH340/CH9102",
    0x0403: "FTDI",
}

EOLS = {"lf": "\n", "crlf": "\r\n", "cr": "\r", "none": ""}


def enable_windows_ansi():
    if os.name == "nt":
        os.system("")  # flips on VT processing in modern consoles


# ------------------------------------------------------------ port picking --
def find_ports():
    ports = list(list_ports.comports())
    # known bridges first
    ports.sort(key=lambda p: (p.vid not in KNOWN_VIDS, p.device))
    return ports


def describe(p):
    tag = KNOWN_VIDS.get(p.vid, "")
    vidpid = f"{p.vid:04x}:{p.pid:04x}" if p.vid is not None else "----:----"
    return f"{p.device:<20} {vidpid}  {p.description}" + (f"  [{tag}]" if tag else "")


def choose_port():
    ports = find_ports()
    if not ports:
        sys.exit("No serial ports found. Is the board plugged in?")
    likely = [p for p in ports if p.vid in KNOWN_VIDS]
    if len(likely) == 1:
        return likely[0].device
    if len(ports) == 1:
        return ports[0].device
    print("Multiple serial ports found:")
    for i, p in enumerate(ports):
        print(f"  [{i}] {describe(p)}")
    while True:
        try:
            choice = input("Select port number: ").strip()
            return ports[int(choice)].device
        except (ValueError, IndexError):
            print("Invalid choice.")
        except (EOFError, KeyboardInterrupt):
            sys.exit(0)


# ---------------------------------------------------------------- terminal --
class Terminal:
    def __init__(self, port, baud, timestamps, eol, logfile, hold_lines):
        self.port = port
        self.baud = baud
        self.timestamps = timestamps
        self.eol = eol
        self.hold_lines = hold_lines
        self.ser = None
        self.ser_lock = threading.Lock()
        self.out_lock = threading.Lock()
        self.stop = threading.Event()
        self.log = None
        self.midline = False
        self.cur_color = ""
        self.connected_once = False
        if logfile:
            self.start_log(logfile)

    # ---- output helpers
    def say(self, msg):
        """Print a message from this app (not from the device)."""
        with self.out_lock:
            if self.midline:
                sys.stdout.write("\n")
                self.midline = False
            sys.stdout.write(f"{INFO}[esp_term] {msg}{RESET}\n")
            sys.stdout.flush()
        self._log_write(f"[esp_term] {msg}\n")

    def _log_write(self, text):
        if self.log:
            self.log.write(text)
            self.log.flush()

    def start_log(self, path):
        self.stop_log()
        try:
            self.log = open(path, "a", encoding="utf-8")
            self.say(f"logging to {os.path.abspath(path)}")
        except OSError as e:
            self.log = None
            self.say(f"could not open log file: {e}")

    def stop_log(self):
        if self.log:
            self.log.close()
            self.log = None
            self.say("logging stopped")

    def emit(self, raw, partial=False):
        text = raw.decode("utf-8", errors="replace").replace("\r", "")
        stamp = ""
        if not self.midline:
            if self.timestamps:
                now = datetime.datetime.now()
                stamp = now.strftime("%H:%M:%S.") + f"{now.microsecond // 1000:03d} "
            plain = ANSI_RE.sub("", text)
            color = ""
            if ALERT_RE.search(plain):
                color = ALERT
            elif "\x1b[" not in text:  # device already colored it? leave alone
                m = LOG_RE.match(plain)
                if m:
                    color = COLORS[m.group(1)]
            self.cur_color = color
        color = self.cur_color
        with self.out_lock:
            line = ""
            if stamp:
                line += f"{DIM}{stamp}{RESET}"
            line += f"{color}{text}{RESET if color else ''}"
            if not partial:
                line += "\n"
            sys.stdout.write(line)
            sys.stdout.flush()
            self.midline = partial
        self._log_write((stamp + ANSI_RE.sub("", text)) + ("" if partial else "\n"))

    # ---- serial management
    def open_port(self):
        s = serial.Serial()
        s.port = self.port
        s.baudrate = self.baud
        s.timeout = 0.05
        if self.hold_lines:
            # keep EN/GPIO0 released so opening the port doesn't reset the board
            s.dtr = False
            s.rts = False
        s.open()
        if self.hold_lines:
            s.dtr = False
            s.rts = False
        with self.ser_lock:
            self.ser = s
        self.say(f"connected to {self.port} @ {self.baud} baud"
                 + ("" if self.connected_once else "   (type /help for commands)"))
        self.connected_once = True

    def close_port(self):
        with self.ser_lock:
            if self.ser:
                try:
                    self.ser.close()
                except Exception:
                    pass
                self.ser = None

    def write(self, data: bytes):
        with self.ser_lock:
            if not self.ser or not self.ser.is_open:
                return False
            try:
                self.ser.write(data)
                return True
            except (serial.SerialException, OSError):
                return False

    def reset_chip(self, bootloader=False):
        with self.ser_lock:
            s = self.ser
            if not s:
                return False
            try:
                if bootloader:
                    # classic auto-download sequence: DTR->GPIO0, RTS->EN
                    s.dtr = False
                    s.rts = True
                    time.sleep(0.1)
                    s.dtr = True
                    s.rts = False
                    time.sleep(0.05)
                    s.dtr = False
                else:
                    s.dtr = False
                    s.rts = True
                    time.sleep(0.1)
                    s.rts = False
                return True
            except (serial.SerialException, OSError):
                return False

    # ---- reader thread
    def reader(self):
        buf = b""
        last_rx = time.time()
        warned = False
        while not self.stop.is_set():
            if self.ser is None:
                try:
                    self.open_port()
                    warned = False
                    buf = b""
                except (serial.SerialException, OSError):
                    if not warned:
                        self.say(f"waiting for {self.port} ...")
                        warned = True
                    time.sleep(0.5)
                    continue
            try:
                with self.ser_lock:
                    s = self.ser
                data = s.read(s.in_waiting or 1) if s else b""
            except (serial.SerialException, OSError, TypeError):
                if buf:
                    self.emit(buf, partial=False)
                    buf = b""
                self.say("device disconnected")
                self.close_port()
                time.sleep(0.5)
                continue

            if data:
                last_rx = time.time()
                buf += data
                while b"\n" in buf:
                    line, buf = buf.split(b"\n", 1)
                    self.emit(line)
            elif buf and time.time() - last_rx > 0.2:
                # prompt without newline (e.g. "> ") - show it right away
                self.emit(buf, partial=True)
                buf = b""

    # ---- commands
    HELP = """\
commands (everything else you type is sent to the device):
  /reset            reset the chip (RTS -> EN)
  /boot             reset into download/bootloader mode (DTR/RTS)
  /baud <rate>      change baud rate
  /ts               toggle timestamps
  /log <file>       start logging to a file   (/log off to stop)
  /eol <lf|crlf|cr|none>   line ending appended to sent text
  /hex <bytes>      send raw bytes, e.g. /hex 03 0d 0a
  /ports            list serial ports
  /clear            clear the screen
  /quit             exit  (Ctrl+C / Ctrl+D also work)"""

    def command(self, line):
        parts = line[1:].split()
        if not parts:
            return True
        cmd, args = parts[0].lower(), parts[1:]

        if cmd in ("q", "quit", "exit"):
            return False
        elif cmd in ("h", "help", "?"):
            print(self.HELP)
        elif cmd in ("r", "reset"):
            self.say("resetting..." if self.reset_chip() else "not connected")
        elif cmd in ("b", "boot", "bootloader"):
            if self.reset_chip(bootloader=True):
                self.say("entered download mode (if your board supports auto-reset)")
            else:
                self.say("not connected")
        elif cmd == "baud":
            try:
                rate = int(args[0])
                self.baud = rate
                with self.ser_lock:
                    if self.ser:
                        self.ser.baudrate = rate
                self.say(f"baud rate set to {rate}")
            except (IndexError, ValueError):
                self.say("usage: /baud <rate>")
        elif cmd in ("ts", "time", "timestamps"):
            self.timestamps = not self.timestamps
            self.say(f"timestamps {'on' if self.timestamps else 'off'}")
        elif cmd == "log":
            if not args or args[0].lower() == "off":
                self.stop_log()
            else:
                self.start_log(" ".join(args))
        elif cmd == "eol":
            if args and args[0].lower() in EOLS:
                self.eol = EOLS[args[0].lower()]
                self.say(f"line ending: {args[0].lower()}")
            else:
                self.say("usage: /eol <lf|crlf|cr|none>")
        elif cmd == "hex":
            try:
                data = bytes(int(x, 16) for x in args)
                self.write(data)
            except ValueError:
                self.say("usage: /hex 03 0d 0a")
        elif cmd == "ports":
            for p in find_ports():
                print("  " + describe(p))
        elif cmd == "clear":
            os.system("cls" if os.name == "nt" else "clear")
        else:
            self.say(f"unknown command '/{cmd}' - try /help")
        return True

    def run(self):
        t = threading.Thread(target=self.reader, daemon=True)
        t.start()
        try:
            while True:
                try:
                    line = input()
                except EOFError:
                    break
                with self.out_lock:
                    self.midline = False
                if line.startswith("/") and not line.startswith("//"):
                    if not self.command(line):
                        break
                else:
                    if line.startswith("//"):
                        line = line[1:]  # "//foo" sends "/foo"
                    if not self.write((line + self.eol).encode("utf-8")):
                        self.say("not connected - message not sent")
        except KeyboardInterrupt:
            pass
        finally:
            self.stop.set()
            self.close_port()
            if self.log:
                self.log.close()
            print()


def main():
    ap = argparse.ArgumentParser(description="Serial terminal for ESP32 debugging")
    ap.add_argument("-p", "--port", help="serial port (default: auto-detect)")
    ap.add_argument("-b", "--baud", type=int, default=115200, help="baud rate (default 115200)")
    ap.add_argument("-t", "--timestamps", action="store_true", help="show timestamps")
    ap.add_argument("-l", "--log", metavar="FILE", help="also log output to FILE")
    ap.add_argument("--eol", choices=EOLS.keys(), default="lf",
                    help="line ending for sent text (default lf)")
    ap.add_argument("--reset", action="store_true",
                    help="allow the board to reset when the port opens "
                         "(default: DTR/RTS held low to avoid it)")
    ap.add_argument("--list", action="store_true", help="list ports and exit")
    args = ap.parse_args()

    enable_windows_ansi()

    if args.list:
        for p in find_ports():
            print(describe(p))
        return

    port = args.port or choose_port()
    term = Terminal(
        port=port,
        baud=args.baud,
        timestamps=args.timestamps,
        eol=EOLS[args.eol],
        logfile=args.log,
        hold_lines=not args.reset,
    )
    term.run()


if __name__ == "__main__":
    main()
