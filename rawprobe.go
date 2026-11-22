package main

// `rex-glance rawprobe`: sends a few kitty graphics commands with answers on
// and prints exactly what comes back, escaped, to see what a terminal supports.

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"time"

	"golang.org/x/term"
)

func rawprobe() error {
	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return err
	}
	defer term.Restore(fd, old)
	in := newReader()
	say := func(f string, a ...any) { fmt.Printf(f+"\r\n", a...) }
	drain := func(d time.Duration) string {
		var got []byte
		t := time.After(d)
		for {
			select {
			case b, ok := <-in.ch:
				if !ok {
					return strconv.Quote(string(got))
				}
				got = append(got, b...)
			case <-t:
				return strconv.Quote(string(got))
			}
		}
	}
	tests := []struct {
		name, keys string
		w, h       int
	}{
		{"transmit+display (a=T)", "a=T,i=9,f=24,s=32,v=32,C=1", 32, 32},
		{"frame edit of frame 1 (a=f,r=1)", "a=f,i=9,r=1,x=0,y=0,s=8,v=8,f=24", 8, 8},
		{"new frame (a=f, no r)", "a=f,i=9,x=0,y=0,s=8,v=8,f=24", 8, 8},
		{"replace data, same id (a=T again)", "a=T,i=9,f=24,s=32,v=32,C=1", 32, 32},
		{"transmit only (a=t)", "a=t,i=10,f=24,s=32,v=32", 32, 32},
		{"put placement (a=p)", "a=p,i=10,C=1", 0, 0},
	}
	for _, tc := range tests {
		var b bytes.Buffer
		if tc.w > 0 {
			kitty(&b, tc.keys, solid(tc.w, tc.h, 200, 60, 60))
		} else {
			fmt.Fprintf(&b, "\x1b_G%s\x1b\\", tc.keys)
		}
		os.Stdout.Write(b.Bytes())
		say("%-36s → %s", tc.name, drain(1500*time.Millisecond))
	}
	say("done; press a key")
	<-in.ch
	return nil
}
