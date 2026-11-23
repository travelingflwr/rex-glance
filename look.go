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
	"sync/atomic"
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
	fast := fs.Bool("fast", false, "draw at the pane's point size: less sharp on Retina, about a quarter of the data")
	ask := fs.Bool("ask", false, "ask for the password even if one is saved")
	choose := fs.Bool("choose", false, "ask here which machine, from the ones given as Name=host")
	host := ""
	// The machine may come before or after the flags.
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		host, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *choose {
		specs := fs.Args()
		if host != "" {
			specs = append([]string{host}, specs...)
		}
		ch, err := chooseMachine(parseChoices(specs))
		if errors.Is(err, errChoseNothing) {
			return nil
		}
		if err != nil {
			return err
		}
		host = ch.host
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
	density := 2
	if *fast {
		density = 1
	}
	return view(c, fb, host, density)
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
	mu   sync.Mutex
	out  io.Writer
	fb   *framebuffer
	host string

	// Where the picture sits: imgW×imgH pane points (what the mouse reports
	// in), covering cols×crows cells. It's drawn at rW×rH pixels, up to
	// density times that, and the Rex app fits it to the cells, so text stays
	// sharp on a Retina screen.
	imgW, imgH  int
	cols, crows int
	rW, rH      int
	density     int
	rows        int
	xs0, xs1    []int // for each drawn column, the screen columns it averages
	ys0, ys1    []int
	shown       []byte // the framebuffer as last drawn, to skip what didn\'t really change

	frames    int
	bytesSent int
	since     time.Time
	mac       bool // a Mac: Ctrl+Option sends ⌘
	degraded  bool // dropped to 1× because drawing was slow
}

// layout fits the remote screen into the pane, keeping its shape and leaving
// the last row for the status line. It fills the pane even when that's larger
// than the remote screen: on a high-resolution iPad, the screen's own pixels
// would cover only part of it, and bigger is easier to click.
func (s *screen) layout() {
	cols, rows, pw, ph := 80, 24, 640, 384
	if ws, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ); err == nil && ws.Col > 0 && ws.Row > 1 {
		cols, rows = int(ws.Col), int(ws.Row)
		if ws.Xpixel > 0 && ws.Ypixel > 0 {
			pw, ph = int(ws.Xpixel), int(ws.Ypixel)
		} else {
			pw, ph = cols*8, rows*16
		}
	}
	cw, ch := max(1, pw/cols), max(1, ph/rows)
	availW, availH := float64(cols*cw), float64((rows-1)*ch)
	scale := min(availW/float64(s.fb.W), availH/float64(s.fb.H))
	c := clamp(int(float64(s.fb.W)*scale/float64(cw)+0.5), 1, cols)
	r := clamp(int(float64(s.fb.H)*scale/float64(ch)+0.5), 1, rows-1)
	s.cols, s.crows, s.rows = c, r, rows
	s.imgW, s.imgH = c*cw, r*ch
	d := max(1, s.density)
	s.rW, s.rH = min(s.fb.W, s.imgW*d), min(s.fb.H, s.imgH*d)
	s.buildMaps()
}

// buildMaps works out, once per layout, which screen pixels each drawn pixel
// averages.
func (s *screen) buildMaps() {
	span := func(n, src int) ([]int, []int) {
		a, b := make([]int, n), make([]int, n)
		for i := 0; i < n; i++ {
			a[i] = i * src / n
			b[i] = max(a[i]+1, (i+1)*src/n)
		}
		return a, b
	}
	s.xs0, s.xs1 = span(s.rW, s.fb.W)
	s.ys0, s.ys1 = span(s.rH, s.fb.H)
}

// sample is the drawn area covering the framebuffer rectangle r, as RGB.
// Each drawn pixel is the average of the screen pixels it covers, so thin
// text strokes survive the shrinking.
func (s *screen) sample(r Rect) (ox, oy, ow, oh int, pix []byte) {
	fx := float64(s.rW) / float64(s.fb.W)
	fy := float64(s.rH) / float64(s.fb.H)
	ox, oy = int(float64(r.X)*fx), int(float64(r.Y)*fy)
	ox1 := min(s.rW, int(float64(r.X+r.W)*fx+0.999))
	oy1 := min(s.rH, int(float64(r.Y+r.H)*fy+0.999))
	ow, oh = ox1-ox, oy1-oy
	if ow <= 0 || oh <= 0 {
		return 0, 0, 0, 0, nil
	}
	return ox, oy, ow, oh, s.sampleOut(ox, oy, ow, oh)
}

// sampleOut is the drawn picture's rectangle at ox,oy, ow×oh, as RGB.
func (s *screen) sampleOut(ox, oy, ow, oh int) []byte {
	ox1, oy1 := ox+ow, oy+oh
	pix := make([]byte, ow*oh*3)
	i := 0
	W := s.fb.W
	for y := oy; y < oy1; y++ {
		y0, y1 := s.ys0[y], s.ys1[y]
		for x := ox; x < ox1; x++ {
			x0, x1 := s.xs0[x], s.xs1[x]
			if y1-y0 == 1 && x1-x0 == 1 {
				copy(pix[i:i+3], s.fb.Pix[(y0*W+x0)*3:])
				i += 3
				continue
			}
			var sr, sg, sb, n int
			for yy := y0; yy < y1; yy++ {
				row := (yy*W + x0) * 3
				for xx := x0; xx < x1; xx++ {
					sr += int(s.fb.Pix[row])
					sg += int(s.fb.Pix[row+1])
					sb += int(s.fb.Pix[row+2])
					row += 3
					n++
				}
			}
			pix[i], pix[i+1], pix[i+2] = byte((sr+n/2)/n), byte((sg+n/2)/n), byte((sb+n/2)/n)
			i += 3
		}
	}
	return pix
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
	s.shown = append(s.shown[:0], s.fb.Pix...)
	var b bytes.Buffer
	b.WriteString("\x1b_Ga=d,d=I,i=1,q=2\x1b\\\x1b[H\x1b[2J")
	kitty(&b, fmt.Sprintf("a=T,i=1,f=24,s=%d,v=%d,c=%d,r=%d,o=z,q=2,C=1", w, h, s.cols, s.crows), deflate(pix))
	s.write(b.Bytes())
	s.status()
}

// patch redraws what changed, as edits of image 1 in place. The changes are
// marked on a grid of 32-pixel tiles of the drawn picture, and each run of
// changed tiles along a row is redrawn as one rectangle: a blinking cursor,
// a clock and some typing cost a few tiles, not a box around all three.
func (s *screen) patch(changed []Rect) {
	const tile = 32
	cols, rows := (s.rW+tile-1)/tile, (s.rH+tile-1)/tile
	if cols == 0 || rows == 0 {
		return
	}
	marked := make([]bool, cols*rows)
	fx := float64(s.rW) / float64(s.fb.W)
	fy := float64(s.rH) / float64(s.fb.H)
	for _, r := range changed {
		x0 := clamp(int(float64(r.X)*fx)/tile, 0, cols-1)
		y0 := clamp(int(float64(r.Y)*fy)/tile, 0, rows-1)
		x1 := clamp(int(float64(r.X+r.W)*fx+0.999-1)/tile, 0, cols-1)
		y1 := clamp(int(float64(r.Y+r.H)*fy+0.999-1)/tile, 0, rows-1)
		for ty := y0; ty <= y1; ty++ {
			for tx := x0; tx <= x1; tx++ {
				marked[ty*cols+tx] = true
			}
		}
	}
	// A server may say a large area changed when only a little did (macOS
	// Screen Sharing reports most of a window for a typed letter): keep only
	// the tiles whose pixels differ from what was last drawn.
	if len(s.shown) == len(s.fb.Pix) {
		for ty := 0; ty < rows; ty++ {
			for tx := 0; tx < cols; tx++ {
				if i := ty*cols + tx; marked[i] && !s.tileChanged(tx*tile, ty*tile, tile) {
					marked[i] = false
				}
			}
		}
	} else {
		s.shown = append(s.shown[:0], s.fb.Pix...)
	}
	var b bytes.Buffer
	for ty := 0; ty < rows; ty++ {
		for tx := 0; tx < cols; tx++ {
			if !marked[ty*cols+tx] {
				continue
			}
			run := tx
			for run+1 < cols && marked[ty*cols+run+1] {
				run++
			}
			ox, oy := tx*tile, ty*tile
			w, h := min((run+1)*tile, s.rW)-ox, min((ty+1)*tile, s.rH)-oy
			pix := s.sampleOut(ox, oy, w, h)
			kitty(&b, fmt.Sprintf("a=f,i=1,r=1,x=%d,y=%d,s=%d,v=%d,f=24,o=z,q=2", ox, oy, w, h), deflate(pix))
			s.remember(ox, oy, w, h)
			tx = run
		}
	}
	s.write(b.Bytes())
	s.frames++
}

// source is the framebuffer area a drawn rectangle comes from.
func (s *screen) source(ox, oy, w, h int) (x0, y0, x1, y1 int) {
	ox1, oy1 := min(ox+w, s.rW), min(oy+h, s.rH)
	return s.xs0[ox], s.ys0[oy], s.xs1[ox1-1], s.ys1[oy1-1]
}

// tileChanged says whether the screen pixels under a drawn tile differ from
// what was last drawn there.
func (s *screen) tileChanged(ox, oy, size int) bool {
	x0, y0, x1, y1 := s.source(ox, oy, min(size, s.rW-ox), min(size, s.rH-oy))
	for y := y0; y < y1; y++ {
		i, j := (y*s.fb.W+x0)*3, (y*s.fb.W+x1)*3
		if !bytes.Equal(s.fb.Pix[i:j], s.shown[i:j]) {
			return true
		}
	}
	return false
}

// remember notes the screen pixels under a drawn rectangle as shown.
func (s *screen) remember(ox, oy, w, h int) {
	x0, y0, x1, y1 := s.source(ox, oy, w, h)
	for y := y0; y < y1; y++ {
		i, j := (y*s.fb.W+x0)*3, (y*s.fb.W+x1)*3
		copy(s.shown[i:j], s.fb.Pix[i:j])
	}
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
	keys := "Ctrl-] quits"
	if s.mac {
		keys = "Ctrl+Option+key: ⌘key · Ctrl-] quits"
	}
	if s.degraded {
		keys = "1× (slow link) · " + keys
	}
	line := fmt.Sprintf(" %s · %d×%d · %.0f updates/s · %.0f KB/s · %s ",
		s.host, s.fb.W, s.fb.H, float64(s.frames)/max(secs, 1), float64(s.bytesSent)/1024/max(secs, 1), keys)
	s.write([]byte(fmt.Sprintf("\x1b7\x1b[%d;1H\x1b[2K\x1b[7m%s\x1b[0m\x1b8", s.rows, line)))
}

func view(c *rfbConn, fb *framebuffer, host string, density int) error {
	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("look needs a terminal (or use --snapshot): %w", err)
	}
	defer term.Restore(fd, old)
	s := &screen{out: os.Stdout, fb: fb, host: host, density: density, mac: c.Mac, since: time.Now()}
	openLog(host)
	glog.printf("screen %dx%d, mac=%v, density %d", fb.W, fb.H, c.Mac, density)
	os.Stdout.WriteString("\x1b]2;Glance: " + host + "\x07") // the pane's title
	os.Stdout.WriteString("\x1b[?25l")                       // hide the cursor
	defer os.Stdout.WriteString("\x1b_Ga=d,d=I,i=1,q=2\x1b\\\x1b[?25h\x1b[H\x1b[2J")

	quit := make(chan error, 2)
	defer func() { glog.printf("view ended") }()
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
				if err := s.send(c, ms, commandChord(ev, c.Mac)); err != nil {
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
	// The screen, in two parts that never wait for each other. The reader
	// applies each update to the framebuffer, notes what changed, and asks for
	// the next one at once. The drawer paints what changed since it last drew:
	// on a slow link it paints less often, but always the newest picture, and
	// the server's stream never stalls behind it.
	var lastUpdate atomic.Int64
	lastUpdate.Store(time.Now().UnixNano())
	c.resizeLock = &s.mu
	var dirtyMu sync.Mutex
	var dirty []Rect
	needFull := false
	wake := make(chan struct{}, 1)
	go func() {
		n := 0
		for {
			t0 := time.Now()
			changed, resized, err := c.next(fb)
			if err != nil {
				glog.printf("read error: %v", err)
				quit <- err
				return
			}
			lastUpdate.Store(time.Now().UnixNano())
			if resized {
				err = c.request(false)
			} else {
				err = c.request(true)
			}
			if err != nil {
				quit <- err
				return
			}
			dirtyMu.Lock()
			dirty = append(dirty, changed...)
			needFull = needFull || resized
			dirtyMu.Unlock()
			select {
			case wake <- struct{}{}:
			default:
			}
			n++
			if n <= 10 || n%100 == 0 {
				glog.printf("update %d: %d rects, resized=%v, waited %s", n, len(changed), resized, time.Since(t0).Round(time.Millisecond))
			}
		}
	}()
	go func() {
		tick := time.Now()
		slow := 0
		for range wake {
			dirtyMu.Lock()
			rects, full := dirty, needFull
			dirty, needFull = nil, false
			dirtyMu.Unlock()
			t0 := time.Now()
			s.mu.Lock()
			sent0 := s.bytesSent
			switch {
			case full:
				s.full()
			case len(rects) > 0:
				s.patch(rects)
			}
			if time.Since(tick) > time.Second {
				s.status()
				tick = time.Now()
			}
			sent := s.bytesSent - sent0
			d := time.Since(t0)
			// Drawing is slow when the link to the screen you're looking at
			// can't keep up: after a few slow draws in a row, draw at the
			// pane's point size, a quarter of the data.
			if d > 250*time.Millisecond {
				slow++
			} else {
				slow = 0
			}
			if slow >= 3 && s.density > 1 {
				s.density, s.degraded = 1, true
				glog.printf("drawing is slow (%s for %d KB): switching to 1×", d.Round(time.Millisecond), sent/1024)
				s.full()
				s.status()
				slow = 0
			}
			s.mu.Unlock()
			if d > 250*time.Millisecond {
				glog.printf("slow draw: %d rects, %d KB in %s", len(rects), sent/1024, d.Round(time.Millisecond))
			}
		}
	}()
	for {
		select {
		case err := <-quit:
			return err
		case <-time.After(5 * time.Second):
			if last := time.Unix(0, lastUpdate.Load()); time.Since(last) > 5*time.Second {
				glog.printf("no update for %s", time.Since(last).Round(time.Second))
			}
		case <-winch:
			s.mu.Lock()
			s.full()
			s.mu.Unlock()
		}
	}
}
