package main

// `rex-glance probe`: run it in a Rex pane to learn what the terminal can do
// for screen sharing. It answers three questions:
//
//  1. How big is the pane in pixels, and does the terminal answer kitty
//     graphics queries?
//  2. Can a rectangle of an image already on screen be replaced in place
//     (a kitty animation-frame edit), and how fast, round trip included?
//  3. Does the terminal report the mouse per pixel (mode 1016) or per cell?

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"time"

	"golang.org/x/term"
)

const esc = "\x1b"

// reader collects what the terminal sends back, from a goroutine, so a
// query can wait for its answer with a timeout.
type reader struct{ ch chan []byte }

func newReader() *reader {
	r := &reader{ch: make(chan []byte, 64)}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				b := make([]byte, n)
				copy(b, buf[:n])
				r.ch <- b
			}
			if err != nil {
				close(r.ch)
				return
			}
		}
	}()
	return r
}

// await reads until re matches what has arrived, or the timeout passes.
func (r *reader) await(re *regexp.Regexp, timeout time.Duration) ([]string, bool) {
	var got []byte
	deadline := time.After(timeout)
	for {
		if m := re.FindSubmatch(got); m != nil {
			out := make([]string, len(m))
			for i := range m {
				out[i] = string(m[i])
			}
			return out, true
		}
		select {
		case b, ok := <-r.ch:
			if !ok {
				return nil, false
			}
			got = append(got, b...)
		case <-deadline:
			return nil, false
		}
	}
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

// kitty writes one graphics command, splitting the payload into the 4096-byte
// chunks the protocol asks for.
func kitty(w *bytes.Buffer, keys string, payload []byte) {
	data := base64.StdEncoding.EncodeToString(payload)
	if len(data) <= 4096 {
		fmt.Fprintf(w, "%s_G%s;%s%s\\", esc, keys, data, esc)
		return
	}
	for i := 0; i < len(data); i += 4096 {
		end := i + 4096
		more := 1
		if end >= len(data) {
			end, more = len(data), 0
		}
		if i == 0 {
			fmt.Fprintf(w, "%s_G%s,m=%d;%s%s\\", esc, keys, more, data[i:end], esc)
		} else {
			fmt.Fprintf(w, "%s_Gm=%d;%s%s\\", esc, more, data[i:end], esc)
		}
	}
}

// solid is w×h RGB pixels of one colour.
func solid(w, h int, r, g, b byte) []byte {
	px := make([]byte, w*h*3)
	for i := 0; i < len(px); i += 3 {
		px[i], px[i+1], px[i+2] = r, g, b
	}
	return px
}

func probe() error {
	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("probe needs a terminal: %w", err)
	}
	defer term.Restore(fd, old)
	in := newReader()
	out := os.Stdout
	say := func(format string, a ...any) { fmt.Fprintf(out, format+"\r\n", a...) }

	fmt.Fprint(out, esc+"[2J"+esc+"[H")
	say("rex-glance probe")
	say("")

	// 1. Sizes.
	cols, rows, _ := term.GetSize(fd)
	fmt.Fprint(out, esc+"[14t")
	pane, okPane := in.await(regexp.MustCompile(`\x1b\[4;(\d+);(\d+)t`), 2*time.Second)
	fmt.Fprint(out, esc+"[16t")
	cell, okCell := in.await(regexp.MustCompile(`\x1b\[6;(\d+);(\d+)t`), 2*time.Second)
	say("1. Size: %d×%d cells", cols, rows)
	if okPane {
		say("   pane: %s×%s px", pane[2], pane[1])
	} else {
		say("   pane: no answer to CSI 14 t")
	}
	if okCell {
		say("   cell: %s×%s px", cell[2], cell[1])
	} else {
		say("   cell: no answer to CSI 16 t")
	}
	var q bytes.Buffer
	kitty(&q, "i=31,s=1,v=1,a=q,t=d,f=24", []byte{0, 0, 0})
	out.Write(q.Bytes())
	if m, ok := in.await(regexp.MustCompile(`\x1b_Gi=31;([^\x1b]*)\x1b\\`), 2*time.Second); ok {
		say("   kitty graphics: %s", m[1])
	} else {
		say("   kitty graphics: no answer")
	}

	// 2. A picture, then tiles repainted in place.
	w, h := 640, 360
	if okCell {
		cw, ch := atoi(cell[2]), atoi(cell[1])
		if cw > 0 && ch > 0 {
			w, h = min(cols*cw, 960), min((rows-14)*ch, 540)
		}
	}
	say("")
	say("2. Drawing a %d×%d picture, then repainting 64×64 tiles inside it…", w, h)
	top := 12
	var b bytes.Buffer
	fmt.Fprintf(&b, "%s[%d;1H", esc, top)
	kitty(&b, fmt.Sprintf("a=T,i=7,f=24,s=%d,v=%d,q=2,C=1", w, h), solid(w, h, 40, 40, 48))
	out.Write(b.Bytes())

	const tiles = 300
	colours := [][3]byte{{220, 80, 80}, {80, 200, 120}, {80, 140, 230}, {230, 190, 70}}
	start := time.Now()
	sent := 0
	for i := 0; i < tiles; i++ {
		b.Reset()
		c := colours[i%len(colours)]
		x := (i * 64) % max(w-64, 64)
		y := ((i * 64) / max(w-64, 64) * 64) % max(h-64, 64)
		keys := fmt.Sprintf("a=f,i=7,r=1,x=%d,y=%d,s=64,v=64,f=24,q=2", x, y)
		if i == tiles-1 {
			keys = fmt.Sprintf("a=f,i=7,r=1,x=%d,y=%d,s=64,v=64,f=24", x, y) // ask for an answer
		}
		kitty(&b, keys, solid(64, 64, c[0], c[1], c[2]))
		sent += b.Len()
		out.Write(b.Bytes())
	}
	ack, okAck := in.await(regexp.MustCompile(`\x1b_Gi=7(?:,r=\d+)?;([^\x1b]*)\x1b\\`), 10*time.Second)
	took := time.Since(start)
	fmt.Fprintf(out, "%s[%d;1H", esc, 9)
	if okAck {
		say("   %d tiles in %s: %.0f tiles/s, %.1f MB/s sent. Answer: %s", tiles, took.Round(time.Millisecond),
			float64(tiles)/took.Seconds(), float64(sent)/took.Seconds()/1e6, ack[1])
	} else {
		say("   %d tiles sent in %s, but no answer came back (frame edits may not be supported)", tiles, took.Round(time.Millisecond))
	}
	// One tile alone, answered: the round trip a single small change costs.
	b.Reset()
	kitty(&b, "a=f,i=7,r=1,x=0,y=0,s=64,v=64,f=24", solid(64, 64, 255, 255, 255))
	t0 := time.Now()
	out.Write(b.Bytes())
	if _, ok := in.await(regexp.MustCompile(`\x1b_Gi=7(?:,r=\d+)?;[^\x1b]*\x1b\\`), 5*time.Second); ok {
		say("   one tile, round trip: %s. Is the picture now patterned, with a white top-left tile?", time.Since(t0).Round(time.Microsecond))
	} else {
		say("   one tile: no answer")
	}

	// 3. The mouse.
	fmt.Fprint(out, esc+"[?1003h"+esc+"[?1006h"+esc+"[?1016h")
	defer fmt.Fprint(out, esc+"[?1016l"+esc+"[?1006l"+esc+"[?1003l")
	say("")
	say("3. Click inside the picture 3 times, near its corners (or press q to skip)…")
	mouse := regexp.MustCompile(`\x1b\[<(\d+);(\d+);(\d+)([Mm])`)
	clicks, maxX, maxY := 0, 0, 0
	var pending []byte
	deadline := time.After(5 * time.Minute)
	for clicks < 3 {
		select {
		case chunk, ok := <-in.ch:
			if !ok {
				return nil
			}
			if bytes.ContainsRune(chunk, 'q') && !bytes.Contains(chunk, []byte(esc)) {
				clicks = 99
				break
			}
			pending = append(pending, chunk...)
			for _, m := range mouse.FindAllSubmatch(pending, -1) {
				x, y := atoi(string(m[2])), atoi(string(m[3]))
				maxX, maxY = max(maxX, x), max(maxY, y)
				if string(m[4]) == "M" && atoi(string(m[1]))&32 == 0 { // a press, not a move
					clicks++
					fmt.Fprintf(out, "%s[%d;1H%s[2K   click %d at %d,%d", esc, 10, esc, clicks, x, y)
				}
			}
			if i := bytes.LastIndexByte(pending, 'M'); i >= 0 {
				pending = pending[i+1:]
			}
		case <-deadline:
			clicks = 99
		}
	}
	fmt.Fprintf(out, "%s[%d;1H", esc, top-1)
	switch {
	case maxX == 0 && maxY == 0:
		say("   No mouse reports arrived.")
	case maxX > cols || maxY > rows:
		say("   Pixel mouse reporting works: positions up to %d,%d, beyond the %d×%d cells.", maxX, maxY, cols, rows)
	default:
		say("   Cell mouse reporting only: positions up to %d,%d within %d×%d cells.", maxX, maxY, cols, rows)
	}
	say("Press any key to finish.")
	<-in.ch
	fmt.Fprint(out, esc+"_Ga=d,d=I,i=7"+esc+"\\")
	return nil
}
