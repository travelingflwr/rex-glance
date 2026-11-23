package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
)

func keys(evs []event) string {
	var out []string
	for _, e := range evs {
		switch {
		case e.quit:
			out = append(out, "quit")
		case e.mouse:
			out = append(out, fmt.Sprintf("mouse(b%d %d,%d press=%v motion=%v)", e.button, e.x, e.y, e.press, e.motion))
		default:
			s := fmt.Sprintf("%#x", e.keysym)
			if e.ctrl {
				s = "C-" + s
			}
			if e.alt {
				s = "A-" + s
			}
			if e.shift {
				s = "S-" + s
			}
			out = append(out, s)
		}
	}
	return strings.Join(out, " ")
}

func TestParseKeys(t *testing.T) {
	cases := map[string]string{
		"ab":        "0x61 0x62",
		"\r":        "0xff0d",
		"\x7f":      "0xff08",
		"\x03":      "C-0x63", // Ctrl-C goes to the machine
		"\x1b[A":    "0xff52",
		"\x1b[1;5C": "C-0xff53",
		"\x1b[1;2D": "S-0xff51",
		"\x1b[3~":   "0xffff",
		"\x1b[5;3~": "A-0xff55",
		"\x1bOP":    "0xffbe",
		"\x1b[15~":  "0xffc2",
		"\x1bx":     "A-0x78",           // Option-x
		"\x1bxab":   "A-0x78 0x61 0x62", // only the next key gets Alt
		"\x1b\r":    "A-0xff0d",
		"\x1b[Z":    "S-0xff09",
		"é":         "0xe9",
		"日":         "0x10065e5",
		"a\x1db":    "0x61 quit 0x62",
	}
	for in, want := range cases {
		evs, used := parseInput([]byte(in), true)
		if got := keys(evs); got != want || used != len(in) {
			t.Errorf("%q: got %q (used %d), want %q", in, got, used, want)
		}
	}
}

func TestEscapeWaitsForTheRestOfASequence(t *testing.T) {
	// A lone Escape: the key only once nothing more is coming.
	if evs, used := parseInput([]byte("\x1b"), false); len(evs) != 0 || used != 0 {
		t.Fatalf("lone Escape, more may come: %v %d", evs, used)
	}
	if evs, _ := parseInput([]byte("\x1b"), true); keys(evs) != "0xff1b" {
		t.Fatalf("lone Escape, nothing more: %v", keys(evs))
	}
	// A mouse report cut in two by a read: nothing typed from its first half.
	first := []byte("x\x1b[<0;10;2")
	evs, used := parseInput(first, false)
	if keys(evs) != "0x78" || used != 1 {
		t.Fatalf("first half: %q used %d", keys(evs), used)
	}
	evs, used = parseInput(append(first[used:], "0M"...), false)
	if keys(evs) != "mouse(b0 9,19 press=true motion=false)" {
		t.Fatalf("joined: %q", keys(evs))
	}
}

func TestParseMouse(t *testing.T) {
	evs, _ := parseInput([]byte("\x1b[<0;101;51M\x1b[<32;120;60M\x1b[<0;120;60m\x1b[<35;5;5M\x1b[<64;7;8M\x1b[<2;1;1M"), true)
	want := "mouse(b0 100,50 press=true motion=false) mouse(b0 119,59 press=true motion=true) " +
		"mouse(b0 119,59 press=false motion=false) mouse(b3 4,4 press=true motion=true) " +
		"mouse(b64 6,7 press=true motion=false) mouse(b2 0,0 press=true motion=false)"
	if got := keys(evs); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestMouseButtonsAndWheel(t *testing.T) {
	m := &mouseState{}
	steps := []struct {
		ev   event
		want []byte
	}{
		{event{mouse: true, button: 0, press: true}, []byte{1}},               // left down
		{event{mouse: true, button: 0, press: true, motion: true}, []byte{1}}, // drag
		{event{mouse: true, button: 0, press: false}, []byte{0}},              // left up
		{event{mouse: true, button: 3, press: true, motion: true}, []byte{0}}, // hover
		{event{mouse: true, button: 64, press: true}, []byte{8, 0}},           // wheel up: click
		{event{mouse: true, button: 2, press: true}, []byte{4}},               // right down
		{event{mouse: true, button: 65, press: true}, []byte{4 | 16, 4}},      // wheel while held
	}
	for i, s := range steps {
		if got := m.pointer(s.ev); !bytes.Equal(got, s.want) {
			t.Fatalf("step %d: %v, want %v", i, got, s.want)
		}
	}
}

type bufConn struct{ bytes.Buffer }

func (*bufConn) Close() error { return nil }

func TestSendMapsThePictureToTheScreen(t *testing.T) {
	w := &bufConn{}
	c := &rfbConn{rw: w, Width: 1000, Height: 500}
	s := &screen{fb: newFramebuffer(1000, 500), imgW: 100, imgH: 50}
	ms := &mouseState{}
	if err := s.send(c, ms, event{mouse: true, x: 50, y: 25, button: 0, press: true}); err != nil {
		t.Fatal(err)
	}
	msg := w.Bytes()
	if len(msg) != 6 || msg[0] != 5 || msg[1] != 1 ||
		binary.BigEndian.Uint16(msg[2:]) != 500 || binary.BigEndian.Uint16(msg[4:]) != 250 {
		t.Fatalf("pointer message % x", msg)
	}
	// A click on the status line, below the picture, goes nowhere.
	w.Reset()
	s.send(c, ms, event{mouse: true, x: 10, y: 60, button: 0, press: true})
	if w.Len() != 0 {
		t.Fatal("a click below the picture was sent")
	}
	// Ctrl-C: Control down, c down, c up, Control up.
	w.Reset()
	s.send(c, ms, event{keysym: 'c', ctrl: true})
	var seq []string
	for b := w.Bytes(); len(b) >= 8; b = b[8:] {
		seq = append(seq, fmt.Sprintf("%v:%#x", b[1] == 1, binary.BigEndian.Uint32(b[4:])))
	}
	if strings.Join(seq, " ") != "true:0xffe3 true:0x63 false:0x63 false:0xffe3" {
		t.Fatalf("keys: %v", seq)
	}
}

func TestParseChoices(t *testing.T) {
	got := parseChoices([]string{"This Mac=localhost", "linuxbox", " = ", "Lab = lab.local "})
	if len(got) != 3 || got[0] != (choice{"This Mac", "localhost"}) || got[1] != (choice{"linuxbox", "linuxbox"}) ||
		got[2] != (choice{"Lab", "lab.local"}) {
		t.Fatalf("%+v", got)
	}
}
