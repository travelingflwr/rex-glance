package main

// The in-window machine chooser, for a Rex app that can't show palette
// questions yet (the iPad's, for now): the window opens straight away and
// asks here. Arrow keys or a number pick; Return opens; Esc or Ctrl-] goes.

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

// choice is "Name=host", or just "host".
type choice struct{ name, host string }

func parseChoices(specs []string) []choice {
	var out []choice
	for _, s := range specs {
		name, host, ok := strings.Cut(s, "=")
		if !ok {
			host = name
		}
		if strings.TrimSpace(host) != "" {
			out = append(out, choice{strings.TrimSpace(name), strings.TrimSpace(host)})
		}
	}
	return out
}

var errChoseNothing = errors.New("nothing chosen")

// chooseMachine asks in the terminal and returns the chosen host.
func chooseMachine(choices []choice) (choice, error) {
	if len(choices) == 0 {
		return choice{}, errors.New("no machines to choose from")
	}
	if len(choices) == 1 {
		return choices[0], nil
	}
	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return choice{}, err
	}
	defer term.Restore(fd, old)
	sel := 0
	draw := func() {
		var b strings.Builder
		b.WriteString("\x1b[H\x1b[2J\x1b[?25lGlance at which machine?\r\n\r\n")
		for i, c := range choices {
			line := fmt.Sprintf(" %d  %s", i+1, c.name)
			if c.host != c.name {
				line += "  \x1b[2m" + c.host + "\x1b[22m"
			}
			if i == sel {
				line = "\x1b[7m" + line + " \x1b[27m"
			}
			b.WriteString(line + "\r\n")
		}
		b.WriteString("\r\n\x1b[2m↑↓ or a number, Return opens, Esc goes\x1b[22m")
		os.Stdout.WriteString(b.String())
	}
	defer os.Stdout.WriteString("\x1b[?25h\x1b[H\x1b[2J")
	buf := make([]byte, 64)
	for {
		draw()
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return choice{}, err
		}
		in := string(buf[:n])
		switch {
		case in == "\r" || in == "\n":
			return choices[sel], nil
		case in == "\x1b" || in == "\x1d" || in == "\x03" || in == "q":
			return choice{}, errChoseNothing
		case in == "\x1b[A" || in == "\x1bOA" || in == "k":
			sel = (sel + len(choices) - 1) % len(choices)
		case in == "\x1b[B" || in == "\x1bOB" || in == "j":
			sel = (sel + 1) % len(choices)
		case len(in) == 1 && in[0] >= '1' && in[0] <= '9':
			if i := int(in[0] - '1'); i < len(choices) {
				return choices[i], nil
			}
		}
	}
}
