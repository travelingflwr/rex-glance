package main

// `rex-glance look <machine>`: connect, log in, and show the screen in this
// pane, redrawing only what changes. --snapshot saves one frame as a PNG
// instead, for checking a connection without a terminal.

import (
	"bytes"
	"compress/zlib"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/png"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// pipeConn joins a command's stdout and stdin into one connection.
type pipeConn struct {
	io.Reader
	io.WriteCloser
	cmd *exec.Cmd
}

func (p *pipeConn) Close() error {
	p.WriteCloser.Close()
	if p.cmd.Process != nil {
		p.cmd.Process.Kill()
	}
	return p.cmd.Wait()
}

// dial reaches the machine's screen-sharing port. It tries ssh's own
// connection to the machine's localhost first (ssh -W: no port opened or
// forwarded anywhere), then, for a server that listens only on its tailnet
// address (wayvnc often does), a direct connection over the tailnet, which
// WireGuard already encrypts. Each is checked by reading the server's
// greeting, which the returned connection hands back first.
func dial(host string, port int) (io.ReadWriteCloser, error) {
	if host == "localhost" || host == "127.0.0.1" {
		return greeted(net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second))
	}
	conn, sshErr := sshTunnel(host, port)
	if sshErr == nil {
		return conn, nil
	}
	conn, err := greeted(net.DialTimeout("tcp", net.JoinHostPort(host, fmt.Sprint(port)), 8*time.Second))
	if err != nil {
		return nil, fmt.Errorf("can't reach %s's screen sharing: through ssh, %v; directly, %v", host, sshErr, err)
	}
	return conn, nil
}

func sshTunnel(host string, port int) (io.ReadWriteCloser, error) {
	cmd := exec.Command("ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=8",
		"-W", fmt.Sprintf("localhost:%d", port), host)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	conn, err := greeted(&pipeConn{Reader: out, WriteCloser: in, cmd: cmd}, nil)
	if err != nil {
		if msg := firstLine(stderr.String()); msg != "" {
			return nil, errors.New(msg)
		}
		return nil, err
	}
	return conn, nil
}

// greetedConn replays the greeting dial already read.
type greetedConn struct {
	io.Reader
	io.ReadWriteCloser
}

func (g *greetedConn) Read(p []byte) (int, error) { return g.Reader.Read(p) }

// greeted reads the server's 12-byte greeting, so a connection that reaches
// nothing fails here, and returns a connection that reads it again first.
func greeted(conn io.ReadWriteCloser, err error) (io.ReadWriteCloser, error) {
	if err != nil {
		return nil, err
	}
	hello := make([]byte, 12)
	done := make(chan error, 1)
	go func() { _, err := io.ReadFull(conn, hello); done <- err }()
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		err = errors.New("no greeting within 10s")
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	return &greetedConn{Reader: io.MultiReader(bytes.NewReader(hello), conn), ReadWriteCloser: conn}, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

// reachCmd dials and reads the greeting: whether the machine's screen
// sharing can be reached, and how, without logging in.
func reachCmd(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: rex-glance reach <machine>")
	}
	start := time.Now()
	conn, err := dial(args[0], 5900)
	if err != nil {
		return err
	}
	defer conn.Close()
	hello := make([]byte, 12)
	io.ReadFull(conn, hello)
	how := "directly"
	if g, ok := conn.(*greetedConn); ok {
		if _, viaSSH := g.ReadWriteCloser.(*pipeConn); viaSSH {
			how = "through ssh"
		}
	}
	fmt.Printf("%s: %s, %s, in %s\n", args[0], strings.TrimSpace(string(hello)), how, time.Since(start).Round(time.Millisecond))
	return nil
}

func lookCmd(args []string) error {
	fs := flag.NewFlagSet("look", flag.ContinueOnError)
	userName := fs.String("user", "", "the account to log in as (default: yours)")
	port := fs.Int("port", 5900, "the screen-sharing port on that machine")
	snapshot := fs.String("snapshot", "", "save one frame to this PNG and exit")
	ask := fs.Bool("ask", false, "ask for the password even if one is saved")
	host := ""
	// The machine may come before or after the flags.
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		host, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if host == "" && fs.NArg() > 0 {
		host = fs.Arg(0)
	}
	if host == "" {
		return errors.New("usage: rex-glance look <machine> [--user U] [--snapshot out.png]")
	}
	if *userName == "" {
		if u, err := user.Current(); err == nil {
			*userName = u.Username
		}
	}

	pw, fromKeychain, save, err := password(*userName, host, *ask)
	if err != nil {
		return err
	}
	conn, err := dial(host, *port)
	if err != nil {
		return err
	}
	defer conn.Close()
	c, err := handshake(conn, *userName, pw)
	if err != nil {
		if fromKeychain && strings.Contains(err.Error(), "login refused") {
			return fmt.Errorf("%w (the saved password; run with --ask to type it again)", err)
		}
		return err
	}
	if save {
		if err := keychainSet(*userName, host, pw); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	}
	fb := newFramebuffer(c.Width, c.Height)
	if *snapshot != "" {
		return snap(c, fb, *snapshot)
	}
	return view(c, fb, host)
}

// snap waits for the first full frame and saves it.
func snap(c *rfbConn, fb *framebuffer, path string) error {
	if err := c.request(false); err != nil {
		return err
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		changed, _, err := c.next(fb)
		if err != nil {
			return err
		}
		if len(changed) > 0 {
			break
		}
	}
	img := image.NewRGBA(image.Rect(0, 0, fb.W, fb.H))
	for i, j := 0, 0; i < len(fb.Pix); i, j = i+3, j+4 {
		img.Pix[j], img.Pix[j+1], img.Pix[j+2], img.Pix[j+3] = fb.Pix[i], fb.Pix[i+1], fb.Pix[i+2], 255
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		return err
	}
	fmt.Printf("%s: %dx%d, saved to %s\n", c.Name, fb.W, fb.H, path)
	return nil
}

// ---- the pane ------------------------------------------------------------

type screen struct {
	mu        sync.Mutex
	out       io.Writer
	fb        *framebuffer
	imgW      int // the picture's size in the pane, in pixels
	imgH      int
	rows      int
	host      string
	frames    int
	bytesSent int
	since     time.Time
}

// layout fits the remote screen into the pane, keeping its shape and leaving
// the last row for the status line. It never enlarges.
func (s *screen) layout() {
	ws, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ)
	pw, ph, rows := 1280, 720, 24
	if err == nil {
		rows = int(ws.Row)
		if ws.Xpixel > 0 && ws.Ypixel > 0 && ws.Row > 1 {
			pw = int(ws.Xpixel)
			ph = int(ws.Ypixel) * (int(ws.Row) - 1) / int(ws.Row)
		}
	}
	scale := min(float64(pw)/float64(s.fb.W), float64(ph)/float64(s.fb.H), 1)
	s.imgW, s.imgH, s.rows = max(1, int(float64(s.fb.W)*scale)), max(1, int(float64(s.fb.H)*scale)), rows
}

// sample is the area of the picture covering the framebuffer rectangle r,
// as RGB, scaled by picking the nearest pixel (fast, and fine for a glance).
func (s *screen) sample(r Rect) (ox, oy, ow, oh int, pix []byte) {
	fx := float64(s.imgW) / float64(s.fb.W)
	fy := float64(s.imgH) / float64(s.fb.H)
	ox, oy = int(float64(r.X)*fx), int(float64(r.Y)*fy)
	ox1 := min(s.imgW, int(float64(r.X+r.W)*fx+0.999))
	oy1 := min(s.imgH, int(float64(r.Y+r.H)*fy+0.999))
	ow, oh = ox1-ox, oy1-oy
	if ow <= 0 || oh <= 0 {
		return 0, 0, 0, 0, nil
	}
	pix = make([]byte, ow*oh*3)
	i := 0
	for y := oy; y < oy1; y++ {
		sy := min(s.fb.H-1, y*s.fb.H/s.imgH)
		row := sy * s.fb.W * 3
		for x := ox; x < ox1; x++ {
			sx := min(s.fb.W-1, x*s.fb.W/s.imgW)
			copy(pix[i:i+3], s.fb.Pix[row+sx*3:])
			i += 3
		}
	}
	return
}

func deflate(b []byte) []byte {
	var buf bytes.Buffer
	w, _ := zlib.NewWriterLevel(&buf, zlib.BestSpeed)
	w.Write(b)
	w.Close()
	return buf.Bytes()
}

// full draws the whole picture as image 1, replacing any earlier one.
func (s *screen) full() {
	s.layout()
	_, _, w, h, pix := s.sample(Rect{0, 0, s.fb.W, s.fb.H})
	var b bytes.Buffer
	b.WriteString("\x1b_Ga=d,d=I,i=1,q=2\x1b\\\x1b[H\x1b[2J")
	kitty(&b, fmt.Sprintf("a=T,i=1,f=24,s=%d,v=%d,o=z,q=2,C=1", w, h), deflate(pix))
	s.write(b.Bytes())
	s.status()
}

// patch redraws the changed rectangles, as edits of image 1 in place.
func (s *screen) patch(changed []Rect) {
	if len(changed) > 32 { // many small changes: one rectangle around them all
		u := changed[0]
		for _, r := range changed[1:] {
			x0, y0 := min(u.X, r.X), min(u.Y, r.Y)
			u = Rect{x0, y0, max(u.X+u.W, r.X+r.W) - x0, max(u.Y+u.H, r.Y+r.H) - y0}
		}
		changed = []Rect{u}
	}
	var b bytes.Buffer
	for _, r := range changed {
		ox, oy, w, h, pix := s.sample(r)
		if pix == nil {
			continue
		}
		kitty(&b, fmt.Sprintf("a=f,i=1,r=1,x=%d,y=%d,s=%d,v=%d,f=24,o=z,q=2", ox, oy, w, h), deflate(pix))
	}
	s.write(b.Bytes())
	s.frames++
}

func (s *screen) write(b []byte) {
	s.out.Write(b)
	s.bytesSent += len(b)
}

// send passes one event to the machine: a mouse report mapped from the
// picture to the screen, or a key with its modifiers pressed around it.
func (s *screen) send(c *rfbConn, ms *mouseState, ev event) error {
	if ev.mouse {
		s.mu.Lock()
		w, h, fbW, fbH := s.imgW, s.imgH, s.fb.W, s.fb.H
		s.mu.Unlock()
		if !ev.motion && ev.press && (ev.x >= w || ev.y >= h) {
			return nil // a click outside the picture (the status line) goes nowhere
		}
		x := clamp(ev.x, 0, w-1) * fbW / max(w, 1)
		y := clamp(ev.y, 0, h-1) * fbH / max(h, 1)
		for _, mask := range ms.pointer(ev) {
			if err := c.pointer(mask, x, y); err != nil {
				return err
			}
		}
		return nil
	}
	var mods []uint32
	for _, m := range []struct {
		on bool
		ks uint32
	}{{ev.ctrl, ksControlL}, {ev.alt, ksAltL}, {ev.shift, ksShiftL}, {ev.super, ksSuperL}} {
		if m.on {
			mods = append(mods, m.ks)
		}
	}
	for _, m := range mods {
		if err := c.key(true, m); err != nil {
			return err
		}
	}
	if err := c.key(true, ev.keysym); err != nil {
		return err
	}
	if err := c.key(false, ev.keysym); err != nil {
		return err
	}
	for i := len(mods) - 1; i >= 0; i-- {
		if err := c.key(false, mods[i]); err != nil {
			return err
		}
	}
	return nil
}

func (s *screen) status() {
	secs := time.Since(s.since).Seconds()
	line := fmt.Sprintf(" %s · %d×%d · %.0f updates/s · %.0f KB/s · Ctrl-] quits ",
		s.host, s.fb.W, s.fb.H, float64(s.frames)/max(secs, 1), float64(s.bytesSent)/1024/max(secs, 1))
	s.write([]byte(fmt.Sprintf("\x1b7\x1b[%d;1H\x1b[2K\x1b[7m%s\x1b[0m\x1b8", s.rows, line)))
}

func view(c *rfbConn, fb *framebuffer, host string) error {
	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("look needs a terminal (or use --snapshot): %w", err)
	}
	defer term.Restore(fd, old)
	s := &screen{out: os.Stdout, fb: fb, host: host, since: time.Now()}
	os.Stdout.WriteString("\x1b]2;Glance: " + host + "\x07") // the pane's title
	os.Stdout.WriteString("\x1b[?25l")                       // hide the cursor
	defer os.Stdout.WriteString("\x1b_Ga=d,d=I,i=1,q=2\x1b\\\x1b[?25h\x1b[H\x1b[2J")

	quit := make(chan error, 2)
	// Mouse reports in pixels: buttons, drags, and moves with no button.
	os.Stdout.WriteString("\x1b[?1003h\x1b[?1006h\x1b[?1016h")
	defer os.Stdout.WriteString("\x1b[?1016l\x1b[?1006l\x1b[?1003l")
	// Clicks and keys go to the machine; Ctrl-] quits. A sequence cut off by
	// a read waits for its rest; a lone Escape is the Escape key once 30 ms
	// pass with nothing after it.
	chunks := make(chan []byte, 64)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				chunks <- append([]byte(nil), buf[:n]...)
			}
			if err != nil {
				close(chunks)
				return
			}
		}
	}()
	go func() {
		var pending []byte
		ms := &mouseState{}
		// handle sends the events in pending, keeping what's unfinished.
		handle := func(final bool) bool {
			evs, used := parseInput(pending, final)
			pending = append(pending[:0], pending[used:]...)
			for _, ev := range evs {
				if ev.quit {
					quit <- nil
					return false
				}
				if err := s.send(c, ms, ev); err != nil {
					quit <- err
					return false
				}
			}
			return true
		}
		for {
			var b []byte
			var ok bool
			if len(pending) > 0 {
				select {
				case b, ok = <-chunks:
				case <-time.After(30 * time.Millisecond):
					if !handle(true) {
						return
					}
					continue
				}
			} else {
				b, ok = <-chunks
			}
			if !ok {
				quit <- nil
				return
			}
			pending = append(pending, b...)
			if !handle(false) {
				return
			}
		}
	}()
	// The pane changed size: lay out and draw again.
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)

	s.mu.Lock()
	s.full()
	s.mu.Unlock()
	if err := c.request(false); err != nil {
		return err
	}
	// The screen: apply each update, draw what changed, ask for the next.
	go func() {
		tick := time.Now()
		for {
			changed, resized, err := c.next(fb)
			if err != nil {
				quit <- err
				return
			}
			s.mu.Lock()
			switch {
			case resized:
				s.full()
			case len(changed) > 0:
				s.patch(changed)
			}
			if time.Since(tick) > time.Second {
				s.status()
				tick = time.Now()
			}
			s.mu.Unlock()
			if resized {
				err = c.request(false)
			} else {
				err = c.request(true)
			}
			if err != nil {
				quit <- err
				return
			}
		}
	}()
	for {
		select {
		case err := <-quit:
			return err
		case <-winch:
			s.mu.Lock()
			s.full()
			s.mu.Unlock()
		}
	}
}
