package main

// Input: what the terminal sends when you click, scroll and type, turned
// into VNC pointer and key events. Mouse reports arrive in SGR pixel mode
// (1006 + 1016), which Rex supports, so a click lands on the pixel you
// pointed at. Keys arrive as bytes and escape sequences and become X keysyms.

import (
	"regexp"
	"strconv"
	"unicode/utf8"
)

// X keysyms, the key codes VNC uses.
const (
	ksBackSpace = 0xff08
	ksTab       = 0xff09
	ksReturn    = 0xff0d
	ksEscape    = 0xff1b
	ksHome      = 0xff50
	ksLeft      = 0xff51
	ksUp        = 0xff52
	ksRight     = 0xff53
	ksDown      = 0xff54
	ksPageUp    = 0xff55
	ksPageDown  = 0xff56
	ksEnd       = 0xff57
	ksInsert    = 0xff63
	ksF1        = 0xffbe
	ksShiftL    = 0xffe1
	ksControlL  = 0xffe3
	ksAltL      = 0xffe9
	ksSuperL    = 0xffeb
	ksDelete    = 0xffff
)

// event is one thing the person did.
type event struct {
	quit bool

	// A key, with the modifiers held for it.
	keysym                  uint32
	shift, ctrl, alt, super bool

	// The mouse, in pane pixels.
	mouse         bool
	x, y          int
	button        int // 0 left, 1 middle, 2 right; 64+ the wheel
	press, motion bool
}

var (
	sgrMouse = regexp.MustCompile(`^\x1b\[<(\d+);(\d+);(\d+)([Mm])`)
	csi      = regexp.MustCompile(`^\x1b\[([0-9;]*)([A-Za-z~])`)
)

// tildeKeys are the keys sent as CSI n ~.
var tildeKeys = map[int]uint32{1: ksHome, 2: ksInsert, 3: ksDelete, 4: ksEnd, 5: ksPageUp, 6: ksPageDown,
	7: ksHome, 8: ksEnd, 15: ksF1 + 4, 17: ksF1 + 5, 18: ksF1 + 6, 19: ksF1 + 7, 20: ksF1 + 8,
	21: ksF1 + 9, 23: ksF1 + 10, 24: ksF1 + 11}

// letterKeys are the keys sent as CSI or SS3 and a letter.
var letterKeys = map[byte]uint32{'A': ksUp, 'B': ksDown, 'C': ksRight, 'D': ksLeft, 'H': ksHome, 'F': ksEnd,
	'P': ksF1, 'Q': ksF1 + 1, 'R': ksF1 + 2, 'S': ksF1 + 3}

// parseInput takes the bytes waiting and returns the events in them, and
// how many bytes it used: an escape sequence cut off at the end is left for
// the next read. final says no more bytes are coming right now, so a lone
// Escape is the Escape key, not the start of a sequence.
func parseInput(b []byte, final bool) (evs []event, used int) {
	for used < len(b) {
		rest := b[used:]
		c := rest[0]
		switch {
		case c == 0x1d: // Ctrl-]: quit, the way telnet does
			evs = append(evs, event{quit: true})
			used++

		case c == 0x1b:
			if len(rest) == 1 {
				if !final {
					return evs, used
				}
				evs = append(evs, event{keysym: ksEscape})
				used++
				continue
			}
			if m := sgrMouse.FindSubmatch(rest); m != nil {
				code, _ := strconv.Atoi(string(m[1]))
				x, _ := strconv.Atoi(string(m[2]))
				y, _ := strconv.Atoi(string(m[3]))
				evs = append(evs, event{mouse: true, x: x - 1, y: y - 1, button: code &^ (4 | 8 | 16 | 32),
					motion: code&32 != 0, press: m[4][0] == 'M',
					shift: code&4 != 0, alt: code&8 != 0, ctrl: code&16 != 0})
				used += len(m[0])
				continue
			}
			if rest[1] == '[' {
				m := csi.FindSubmatch(rest)
				if m == nil {
					if !final && len(rest) < 16 {
						return evs, used // the rest of the sequence is on its way
					}
					used++ // not a sequence we know: drop the Escape
					continue
				}
				params, fin := string(m[1]), m[2][0]
				ev := event{}
				first, mod := splitParams(params)
				if fin == '~' {
					ev.keysym = tildeKeys[first]
				} else {
					ev.keysym = letterKeys[fin]
					if fin == 'Z' { // Shift-Tab
						ev.keysym, ev.shift = ksTab, true
					}
				}
				applyMod(&ev, mod)
				if ev.keysym != 0 {
					evs = append(evs, ev)
				}
				used += len(m[0])
				continue
			}
			if rest[1] == 'O' {
				if len(rest) < 3 {
					if !final {
						return evs, used
					}
					used++
					continue
				}
				if ks := letterKeys[rest[2]]; ks != 0 {
					evs = append(evs, event{keysym: ks})
				}
				used += 3
				continue
			}
			// Escape then one key: that key with Alt (Option) held.
			var one []byte
			if rest[1] < 0x80 {
				one = rest[1:2]
			} else {
				if !utf8.FullRune(rest[1:]) {
					if !final {
						return evs, used
					}
					used++
					continue
				}
				_, n := utf8.DecodeRune(rest[1:])
				one = rest[1 : 1+n]
			}
			sub, _ := parseInput(one, true)
			for i := range sub {
				if !sub[i].mouse && !sub[i].quit {
					sub[i].alt = true
				}
			}
			evs = append(evs, sub...)
			used += 1 + len(one)

		case c == '\r' || c == '\n':
			evs = append(evs, event{keysym: ksReturn})
			used++
		case c == '\t':
			evs = append(evs, event{keysym: ksTab})
			used++
		case c == 0x7f || c == 0x08:
			evs = append(evs, event{keysym: ksBackSpace})
			used++
		case c < 0x20: // Ctrl and a letter
			evs = append(evs, event{keysym: uint32('a' + c - 1), ctrl: true})
			used++

		default:
			r, n := utf8.DecodeRune(rest)
			if r == utf8.RuneError && n <= 1 {
				if !final && !utf8.FullRune(rest) {
					return evs, used
				}
				used++
				continue
			}
			evs = append(evs, event{keysym: runeKeysym(r)})
			used += n
		}
	}
	return evs, used
}

// splitParams reads "1;5" as the key number and the xterm modifier.
func splitParams(p string) (first, mod int) {
	first, mod = 1, 1
	if p == "" {
		return
	}
	parts := regexp.MustCompile(`;`).Split(p, -1)
	if v, err := strconv.Atoi(parts[0]); err == nil {
		first = v
	}
	if len(parts) > 1 {
		if v, err := strconv.Atoi(parts[1]); err == nil {
			mod = v
		}
	}
	return
}

// applyMod reads xterm's modifier number: 1 plus Shift 1, Alt 2, Ctrl 4, Super 8.
func applyMod(ev *event, mod int) {
	m := mod - 1
	ev.shift = ev.shift || m&1 != 0
	ev.alt = ev.alt || m&2 != 0
	ev.ctrl = ev.ctrl || m&4 != 0
	ev.super = ev.super || m&8 != 0
}

// runeKeysym is the keysym for a typed character: Latin-1 as itself, the
// rest as Unicode keysyms.
func runeKeysym(r rune) uint32 {
	if r < 0x100 {
		return uint32(r)
	}
	return 0x01000000 | uint32(r)
}

// vncButtons is the VNC button mask bit for a terminal mouse button.
func vncButton(button int) byte {
	switch button {
	case 0:
		return 1 // left
	case 1:
		return 2 // middle
	case 2:
		return 4 // right
	case 64:
		return 8 // wheel up
	case 65:
		return 16 // wheel down
	case 66:
		return 32 // wheel left
	case 67:
		return 64 // wheel right
	}
	return 0
}

// mouseState turns terminal mouse reports into VNC pointer events, keeping
// which buttons are held between them.
type mouseState struct{ held byte }

// pointer returns the button masks to send for ev, in order: a wheel turn
// is a press and a release; a click or drag is the mask now held.
func (m *mouseState) pointer(ev event) []byte {
	bit := vncButton(ev.button)
	switch {
	case ev.button >= 64: // the wheel: one click, pressed then released
		if !ev.press {
			return nil
		}
		return []byte{m.held | bit, m.held}
	case ev.motion:
		return []byte{m.held}
	case ev.press:
		m.held |= bit
	default:
		m.held &^= bit
	}
	return []byte{m.held}
}
