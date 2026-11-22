package main

// The framebuffer and the encodings that fill it: Raw, CopyRect, Zlib and
// ZRLE, for 32-bit true colour (each pixel B, G, R, unused on the wire).

import (
	"bytes"
	"compress/flate"
	"errors"
	"fmt"
	"io"
)

// framebuffer is the remote screen, RGB, three bytes a pixel.
type framebuffer struct {
	W, H int
	Pix  []byte
}

func newFramebuffer(w, h int) *framebuffer {
	return &framebuffer{W: w, H: h, Pix: make([]byte, w*h*3)}
}

func (fb *framebuffer) resize(w, h int) {
	fb.W, fb.H, fb.Pix = w, h, make([]byte, w*h*3)
}

func (fb *framebuffer) inside(r Rect) bool {
	return r.X >= 0 && r.Y >= 0 && r.W >= 0 && r.H >= 0 && r.X+r.W <= fb.W && r.Y+r.H <= fb.H
}

// put sets one pixel from a wire pixel's B, G, R.
func (fb *framebuffer) put(x, y int, b, g, r byte) {
	i := (y*fb.W + x) * 3
	fb.Pix[i], fb.Pix[i+1], fb.Pix[i+2] = r, g, b
}

// inflater keeps one zlib stream going across rectangles. Servers end each
// rectangle's data with a sync flush, a byte-aligned block boundary, so each
// rectangle can be inflated by a fresh reader primed with the last 32 KB of
// output: the stream's state carries over without a reader that blocks
// waiting for the next rectangle.
type inflater struct {
	started bool
	window  []byte
}

// inflate returns exactly n bytes from the stream, given the next
// rectangle's compressed data.
func (z *inflater) inflate(data []byte, n int) ([]byte, error) {
	if !z.started {
		if len(data) < 2 {
			return nil, errors.New("zlib stream too short")
		}
		data = data[2:] // the zlib header; the rest is raw deflate
		z.started = true
	}
	fr := flate.NewReaderDict(bytes.NewReader(data), z.window)
	out := make([]byte, n)
	if _, err := io.ReadFull(fr, out); err != nil {
		return nil, fmt.Errorf("inflating: %w", err)
	}
	z.window = append(z.window, out...)
	if len(z.window) > 32768 {
		z.window = append([]byte(nil), z.window[len(z.window)-32768:]...)
	}
	return out, nil
}

func (c *rfbConn) decode(fb *framebuffer, r Rect, enc int32) error {
	if !fb.inside(r) {
		return fmt.Errorf("rectangle %v is off the %dx%d screen", r, fb.W, fb.H)
	}
	switch enc {
	case encRaw:
		data, err := c.read(r.W * r.H * 4)
		if err != nil {
			return err
		}
		putPixels(fb, r, data)
		return nil

	case encCopyRect:
		src, err := c.read(4)
		if err != nil {
			return err
		}
		sx, sy := int(src[0])<<8|int(src[1]), int(src[2])<<8|int(src[3])
		if !fb.inside(Rect{sx, sy, r.W, r.H}) {
			return fmt.Errorf("copy source %d,%d is off screen", sx, sy)
		}
		tmp := make([]byte, r.W*r.H*3) // source and destination may overlap
		for y := 0; y < r.H; y++ {
			copy(tmp[y*r.W*3:(y+1)*r.W*3], fb.Pix[((sy+y)*fb.W+sx)*3:])
		}
		for y := 0; y < r.H; y++ {
			copy(fb.Pix[((r.Y+y)*fb.W+r.X)*3:], tmp[y*r.W*3:(y+1)*r.W*3])
		}
		return nil

	case encZlib:
		n, err := c.u32()
		if err != nil {
			return err
		}
		data, err := c.read(int(n))
		if err != nil {
			return err
		}
		pix, err := c.zlib.inflate(data, r.W*r.H*4)
		if err != nil {
			return err
		}
		putPixels(fb, r, pix)
		return nil

	case encZRLE:
		n, err := c.u32()
		if err != nil {
			return err
		}
		data, err := c.read(int(n))
		if err != nil {
			return err
		}
		return c.zrle.zrle(fb, r, data)
	}
	return fmt.Errorf("the server sent encoding %d, which wasn't asked for", enc)
}

func putPixels(fb *framebuffer, r Rect, data []byte) {
	i := 0
	for y := 0; y < r.H; y++ {
		for x := 0; x < r.W; x++ {
			fb.put(r.X+x, r.Y+y, data[i], data[i+1], data[i+2])
			i += 4
		}
	}
}

// zrleReader reads a rectangle's inflated ZRLE data, which arrives as one
// buffer: inflate it all, then walk it.
type zrleReader struct {
	b   []byte
	pos int
	err error
}

func (z *zrleReader) byte() byte {
	if z.pos >= len(z.b) {
		z.err = errors.New("ZRLE data ended early")
		return 0
	}
	v := z.b[z.pos]
	z.pos++
	return v
}

// cpixel is a compressed pixel: with 32-bit true colour of depth 24, the
// three bytes B, G, R.
func (z *zrleReader) cpixel() [3]byte {
	if z.pos+3 > len(z.b) {
		z.err = errors.New("ZRLE data ended early")
		z.pos = len(z.b)
		return [3]byte{}
	}
	p := [3]byte{z.b[z.pos], z.b[z.pos+1], z.b[z.pos+2]}
	z.pos += 3
	return p
}

// zrle decodes one ZRLE rectangle: 64×64 tiles, each raw, solid, packed
// palette, plain run-length or palette run-length. The inflated size isn't
// sent, so inflate everything the rectangle's data holds.
func (z *inflater) zrle(fb *framebuffer, r Rect, data []byte) error {
	raw, err := z.inflateAll(data)
	if err != nil {
		return err
	}
	rd := &zrleReader{b: raw}
	for ty := r.Y; ty < r.Y+r.H; ty += 64 {
		th := min(64, r.Y+r.H-ty)
		for tx := r.X; tx < r.X+r.W; tx += 64 {
			tw := min(64, r.X+r.W-tx)
			if err := zrleTile(fb, rd, tx, ty, tw, th); err != nil {
				return err
			}
		}
	}
	return rd.err
}

// inflateAll is inflate for when the output size isn't known: all the output
// this rectangle's data produces.
func (z *inflater) inflateAll(data []byte) ([]byte, error) {
	if !z.started {
		if len(data) < 2 {
			return nil, errors.New("zlib stream too short")
		}
		data = data[2:]
		z.started = true
	}
	fr := flate.NewReaderDict(bytes.NewReader(data), z.window)
	out, err := io.ReadAll(fr)
	// The data ends at a sync flush, not the stream's end, so running out of
	// input is how a rectangle finishes.
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, fmt.Errorf("inflating: %w", err)
	}
	z.window = append(z.window, out...)
	if len(z.window) > 32768 {
		z.window = append([]byte(nil), z.window[len(z.window)-32768:]...)
	}
	return out, nil
}

func zrleTile(fb *framebuffer, rd *zrleReader, tx, ty, tw, th int) error {
	sub := rd.byte()
	set := func(i int, p [3]byte) { fb.put(tx+i%tw, ty+i/tw, p[0], p[1], p[2]) }
	n := tw * th
	switch {
	case sub == 0: // raw
		for i := 0; i < n && rd.err == nil; i++ {
			set(i, rd.cpixel())
		}
	case sub == 1: // solid
		p := rd.cpixel()
		for i := 0; i < n; i++ {
			set(i, p)
		}
	case sub >= 2 && sub <= 16: // packed palette
		pal := make([][3]byte, sub)
		for i := range pal {
			pal[i] = rd.cpixel()
		}
		bits := 4
		if sub == 2 {
			bits = 1
		} else if sub <= 4 {
			bits = 2
		}
		for y := 0; y < th && rd.err == nil; y++ {
			var cur byte
			left := 0
			for x := 0; x < tw; x++ {
				if left == 0 {
					cur, left = rd.byte(), 8
				}
				left -= bits
				idx := int(cur>>uint(left)) & (1<<bits - 1)
				if idx < len(pal) {
					set(y*tw+x, pal[idx])
				}
			}
		}
	case sub == 128: // plain run-length
		for i := 0; i < n && rd.err == nil; {
			p := rd.cpixel()
			run := runLength(rd)
			for j := 0; j < run && i < n; j++ {
				set(i, p)
				i++
			}
		}
	case sub >= 130: // palette run-length
		pal := make([][3]byte, sub-128)
		for i := range pal {
			pal[i] = rd.cpixel()
		}
		for i := 0; i < n && rd.err == nil; {
			idx := rd.byte()
			run := 1
			if idx&128 != 0 {
				idx &= 127
				run = runLength(rd)
			}
			if int(idx) >= len(pal) {
				return fmt.Errorf("ZRLE palette index %d of %d", idx, len(pal))
			}
			for j := 0; j < run && i < n; j++ {
				set(i, pal[idx])
				i++
			}
		}
	default:
		return fmt.Errorf("ZRLE subencoding %d isn't defined", sub)
	}
	return rd.err
}

// runLength reads a ZRLE run length: bytes of 255 continue, the run is one
// more than their sum.
func runLength(rd *zrleReader) int {
	run := 1
	for rd.err == nil {
		b := rd.byte()
		run += int(b)
		if b != 255 {
			break
		}
	}
	return run
}
