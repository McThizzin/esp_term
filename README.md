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

