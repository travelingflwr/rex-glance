package main

// `rex-glance bench <machine>`: what a machine's screen sharing actually
// does, measured without a picture. It reports the first full frame's time
// and size, which encodings the server sends, and how long a screen update
// takes to arrive after input: it nudges the pointer one pixel back and
// forth (never a click or a key) and times the update that follows.

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os/user"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// countingConn counts the bytes read from a connection.
type countingConn struct {
	io.ReadWriteCloser
	n atomic.Int64
}

func (c *countingConn) Read(p []byte) (int, error) {
	n, err := c.ReadWriteCloser.Read(p)
	c.n.Add(int64(n))
	return n, err
}

var encNames = map[int32]string{encRaw: "Raw", encCopyRect: "CopyRect", encZlib: "Zlib", encZRLE: "ZRLE"}

func benchCmd(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	userName := fs.String("user", "", "the account to log in as (default: yours)")
	secs := fs.Int("seconds", 10, "how long to measure")
	rects := fs.Bool("rects", false, "just watch, printing each update's rectangles (no pointer nudges)")
	host := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		host, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if host == "" {
		return errors.New("usage: rex-glance bench <machine> [--seconds N]")
	}
	if *userName == "" {
		if u, err := user.Current(); err == nil {
			*userName = u.Username
		}
	}
	pw := keychainGet(*userName, host)
	if pw == "" {
		return fmt.Errorf("no saved password for %s on %s: glance at it once and save it", *userName, host)
	}

	t0 := time.Now()
	raw, err := dial(host, 5900)
	if err != nil {
		return err
	}
	conn := &countingConn{ReadWriteCloser: raw}
	defer conn.Close()
	c, err := handshake(conn, *userName, pw)
	if err != nil {
		return err
	}
	connected := time.Since(t0)
	c.EncRects = map[int32]int{}
	fb := newFramebuffer(c.Width, c.Height)

	// The first full frame.
	t1 := time.Now()
	c.request(false)
	for {
		changed, _, err := c.next(fb)
		if err != nil {
			return err
		}
		if len(changed) > 0 {
			break
		}
	}
	first, firstBytes := time.Since(t1), conn.n.Load()
	kind := "Linux/other (" + c.Name + ")"
	if c.Mac {
		kind = "macOS Screen Sharing"
	}
	fmt.Printf("%s: %s, %d×%d\n", host, kind, c.Width, c.Height)
	fmt.Printf("  connect and log in: %s\n", connected.Round(time.Millisecond))
	fmt.Printf("  first full frame:   %s, %.1f MB\n", first.Round(time.Millisecond), float64(firstBytes)/1e6)

	if *rects {
		return watchRects(c, fb, conn, time.Duration(*secs)*time.Second)
	}

	// Updates after input: nudge the pointer, time the update that follows.
	updates := make(chan time.Time, 256)
	failed := make(chan error, 1)
	go func() {
		for {
			changed, _, err := c.next(fb)
			if err != nil {
				failed <- err
				return
			}
			if len(changed) > 0 {
				updates <- time.Now()
			}
			c.request(true)
		}
	}()
	c.request(true)
	startBytes := conn.n.Load()
	var lat []time.Duration
	missed := 0
	x, y := c.Width/2, c.Height/2
	deadline := time.Now().Add(time.Duration(*secs) * time.Second)
	for i := 0; time.Now().Before(deadline); i++ {
		for len(updates) > 0 { // drop updates from before this nudge
			<-updates
		}
		sent := time.Now()
		c.pointer(0, x+i%2, y)
		select {
		case at := <-updates:
			lat = append(lat, at.Sub(sent))
		case err := <-failed:
			return err
		case <-time.After(time.Second):
			missed++
		}
		time.Sleep(200 * time.Millisecond)
	}
	c.pointer(0, x, y)

	fmt.Printf("  pointer nudges:     %d, %d with no update within 1s\n", len(lat)+missed, missed)
	if len(lat) > 0 {
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		fmt.Printf("  input to update:    median %s, 90%% within %s, worst %s\n",
			lat[len(lat)/2].Round(time.Millisecond), lat[len(lat)*9/10].Round(time.Millisecond),
			lat[len(lat)-1].Round(time.Millisecond))
	}
	fmt.Printf("  data while nudging: %.0f KB/s\n", float64(conn.n.Load()-startBytes)/1024/float64(*secs))
	var encs []string
	for e, n := range c.EncRects {
		name := encNames[e]
		if name == "" {
			name = fmt.Sprint(e)
		}
		encs = append(encs, fmt.Sprintf("%s %d", name, n))
	}
	sort.Strings(encs)
	fmt.Printf("  encodings sent:     %s\n", strings.Join(encs, ", "))
	return nil
}

// watchRects prints each update's rectangles for a while: what the server
// sends when left alone (or when someone else is using the machine).
func watchRects(c *rfbConn, fb *framebuffer, conn *countingConn, d time.Duration) error {
	c.request(true)
	deadline := time.Now().Add(d)
	start := time.Now()
	for time.Now().Before(deadline) {
		before := conn.n.Load()
		changed, _, err := c.next(fb)
		if err != nil {
			return err
		}
		c.request(true)
		if len(changed) == 0 {
			continue
		}
		var parts []string
		area := 0
		for _, r := range changed {
			parts = append(parts, fmt.Sprintf("%dx%d@%d,%d", r.W, r.H, r.X, r.Y))
			area += r.W * r.H
		}
		if len(parts) > 6 {
			parts = append(parts[:6], fmt.Sprintf("+%d more", len(parts)-6))
		}
		fmt.Printf("  %6.2fs  %3d%% of screen, %5.0f KB: %s\n", time.Since(start).Seconds(),
			100*area/(fb.W*fb.H), float64(conn.n.Load()-before)/1024, strings.Join(parts, " "))
	}
	var encs []string
	for e, n := range c.EncRects {
		name := encNames[e]
		if name == "" {
			name = fmt.Sprint(e)
		}
		encs = append(encs, fmt.Sprintf("%s %d", name, n))
	}
	sort.Strings(encs)
	fmt.Printf("  encodings: %s\n", strings.Join(encs, ", "))
	return nil
}
