package main

import (
	"bytes"
	"compress/zlib"
	"crypto/aes"
	"crypto/md5"
	"crypto/rand"
	"encoding/binary"
	"io"
	"math/big"
	"net"
	"os/exec"
	"testing"
)

// The 1024-bit MODP group (RFC 2409, group 2), as Apple's servers use.
var testPrime, _ = new(big.Int).SetString(
	"FFFFFFFFFFFFFFFFC90FDAA22168C234C4C6628B80DC1CD129024E088A67CC74020BBEA63B139B22514A08798E3404DD"+
		"EF9519B3CD3A431B302B0A6DF25F14374FE1356D6D51C245E485B576625E7EC6F44C42E9A637ED6B0BFF5CB6F406B7ED"+
		"EE386BFB5A899FA5AE9F24117C4B1FE649286651ECE65381FFFFFFFFFFFFFFFF", 16)

// fakeServer plays a VNC server on the other end of a pipe: it checks the
// Apple login, then sends the updates in send.
type fakeServer struct {
	t        *testing.T
	user, pw string
	refuse   bool
	send     [][]byte
}

func (f *fakeServer) run(conn net.Conn) {
	defer conn.Close()
	r := func(n int) []byte {
		b := make([]byte, n)
		if _, err := io.ReadFull(conn, b); err != nil {
			f.t.Errorf("server read: %v", err)
		}
		return b
	}
	conn.Write([]byte("RFB 003.889\n"))
	if v := string(r(12)); v != "RFB 003.008\n" {
		f.t.Errorf("client version %q", v)
	}
	conn.Write([]byte{2, 30, 35})
	if t := r(1)[0]; t != 30 {
		f.t.Errorf("client chose type %d", t)
		return
	}
	// The Apple login, server side.
	keyLen := 128
	priv, _ := rand.Int(rand.Reader, testPrime)
	pub := new(big.Int).Exp(big.NewInt(2), priv, testPrime)
	hdr := []byte{0, 2, byte(keyLen >> 8), byte(keyLen)}
	conn.Write(append(append(hdr, leftPad(testPrime.Bytes(), keyLen)...), leftPad(pub.Bytes(), keyLen)...))
	ct := r(128)
	clientPub := new(big.Int).SetBytes(r(keyLen))
	secret := new(big.Int).Exp(clientPub, priv, testPrime)
	key := md5.Sum(leftPad(secret.Bytes(), keyLen))
	block, _ := aes.NewCipher(key[:])
	plain := make([]byte, 128)
	for i := 0; i < 128; i += 16 {
		block.Decrypt(plain[i:i+16], ct[i:i+16])
	}
	gotUser := string(plain[:bytes.IndexByte(plain, 0)])
	gotPw := string(plain[64 : 64+bytes.IndexByte(plain[64:], 0)])
	if f.refuse || gotUser != f.user || gotPw != f.pw {
		msg := "Authentication failed"
		conn.Write(binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(nil, 1), uint32(len(msg))))
		conn.Write([]byte(msg))
		return
	}
	conn.Write([]byte{0, 0, 0, 0})
	r(1)                       // ClientInit
	init := []byte{0, 8, 0, 4} // an 8×4 screen
	init = append(init, make([]byte, 16)...)
	init = append(init, 0, 0, 0, 4)
	init = append(init, "fake"...)
	conn.Write(init)
	r(20)      // SetPixelFormat
	r(4 + 5*4) // SetEncodings: five
	r(10)      // the first update request
	for _, m := range f.send {
		conn.Write(m)
		r(10) // the next request
	}
}

func pipe(t *testing.T, f *fakeServer) net.Conn {
	f.t = t
	client, server := net.Pipe()
	go f.run(server)
	return client
}

func TestAppleLoginAndInit(t *testing.T) {
	conn := pipe(t, &fakeServer{user: "pat", pw: "s3cret"})
	c, err := handshake(conn, "pat", "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	if c.Width != 8 || c.Height != 4 || c.Name != "fake" || !c.Mac {
		t.Fatalf("%dx%d %q", c.Width, c.Height, c.Name)
	}
}

func TestAppleLoginRefused(t *testing.T) {
	conn := pipe(t, &fakeServer{user: "pat", pw: "s3cret"})
	_, err := handshake(conn, "pat", "wrong")
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("Authentication failed")) {
		t.Fatalf("want the server's reason, got %v", err)
	}
}

// update builds a FramebufferUpdate message from rectangles already encoded.
func update(rects ...[]byte) []byte {
	m := []byte{0, 0, 0, byte(len(rects))}
	for _, r := range rects {
		m = append(m, r...)
	}
	return m
}

func rectHeader(x, y, w, h int, enc int32) []byte {
	b := []byte{}
	for _, v := range []int{x, y, w, h} {
		b = binary.BigEndian.AppendUint16(b, uint16(v))
	}
	return binary.BigEndian.AppendUint32(b, uint32(enc))
}

// zstream is a server's ZRLE zlib stream: each rectangle's data ends with a
// sync flush, the way real servers write it.
type zstream struct {
	buf bytes.Buffer
	w   *zlib.Writer
}

func newZstream() *zstream {
	z := &zstream{}
	z.w = zlib.NewWriter(&z.buf)
	return z
}

func (z *zstream) rect(raw []byte) []byte {
	z.w.Write(raw)
	z.w.Flush() // a sync flush
	out := append([]byte(nil), z.buf.Bytes()...)
	z.buf.Reset()
	return append(binary.BigEndian.AppendUint32(nil, uint32(len(out))), out...)
}

func TestDecodersFillTheFramebuffer(t *testing.T) {
	green, blue := []byte{0, 255, 0}, []byte{255, 0, 0} // B,G,R on the wire

	// Raw: the whole 8×4 screen red.
	raw := rectHeader(0, 0, 8, 4, encRaw)
	for i := 0; i < 32; i++ {
		raw = append(raw, 0, 0, 255, 0)
	}

	zs := newZstream()
	// ZRLE rect 1: a 4×2 solid green tile at 0,0.
	z1 := append(rectHeader(0, 0, 4, 2, encZRLE), zs.rect(append([]byte{1}, green...))...)
	// ZRLE rect 2, same stream: a 4×2 palette-RLE tile at 4,2, blue then green.
	tile := []byte{130}
	tile = append(tile, blue...)
	tile = append(tile, green...)
	tile = append(tile, 0|128, 3, 1|128, 3) // index 0 for 4 pixels, index 1 for 4 (a run is 1 + the byte)
	z2 := append(rectHeader(4, 2, 4, 2, encZRLE), zs.rect(tile)...)
	// ZRLE rect 3: a 2×1 packed-palette tile at 0,3, green then blue (1 bit each).
	pp := []byte{2}
	pp = append(pp, green...)
	pp = append(pp, blue...)
	pp = append(pp, 0b01000000)
	z3 := append(rectHeader(0, 3, 2, 1, encZRLE), zs.rect(pp)...)
	// CopyRect: 1×1 from 0,0 (green) to 7,0.
	cr := append(rectHeader(7, 0, 1, 1, encCopyRect), 0, 0, 0, 0)

	f := &fakeServer{user: "u", pw: "p", send: [][]byte{update(raw), update(z1, z2), update(z3, cr)}}
	c, err := handshake(pipe(t, f), "u", "p")
	if err != nil {
		t.Fatal(err)
	}
	fb := newFramebuffer(c.Width, c.Height)
	if err := c.request(false); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, _, err := c.next(fb); err != nil {
			t.Fatalf("update %d: %v", i, err)
		}
		c.request(true)
	}
	at := func(x, y int) string {
		i := (y*fb.W + x) * 3
		switch {
		case bytes.Equal(fb.Pix[i:i+3], []byte{255, 0, 0}):
			return "R"
		case bytes.Equal(fb.Pix[i:i+3], []byte{0, 255, 0}):
			return "G"
		case bytes.Equal(fb.Pix[i:i+3], []byte{0, 0, 255}):
			return "B"
		}
		return "?"
	}
	var got []string
	for y := 0; y < fb.H; y++ {
		row := ""
		for x := 0; x < fb.W; x++ {
			row += at(x, y)
		}
		got = append(got, row)
	}
	want := []string{
		"GGGGRRRG", // solid green tile, copied pixel at the end
		"GGGGRRRR",
		"RRRRBBBB", // palette RLE: four blue…
		"GBRRGGGG", // …then four green; packed palette at 0,3: green, blue
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("framebuffer:\n%v\nwant:\n%v", got, want)
		}
	}
}

func TestScaledSample(t *testing.T) {
	fb := newFramebuffer(4, 2)
	for i := range fb.Pix {
		fb.Pix[i] = byte(i)
	}
	s := &screen{fb: fb, rW: 2, rH: 1}
	s.buildMaps()
	ox, oy, w, h, pix := s.sample(Rect{2, 0, 2, 2})
	// The right half: pixels (2,0) (3,0) (2,1) (3,1), whose first bytes are
	// 6, 9, 18 and 21, averaged and rounded: 14.
	if ox != 1 || oy != 0 || w != 1 || h != 1 || len(pix) != 3 || pix[0] != 14 {
		t.Fatalf("%d,%d %dx%d %v", ox, oy, w, h, pix)
	}
	// Drawn at full size, a pixel is copied as it is.
	s = &screen{fb: fb, rW: 4, rH: 2}
	s.buildMaps()
	if _, _, _, _, pix := s.sample(Rect{1, 1, 1, 1}); pix[0] != fb.Pix[(1*4+1)*3] {
		t.Fatalf("copied %v", pix)
	}
}

// Changes far apart redraw as separate small rectangles, not one box around
// them all.
func TestPatchDrawsOnlyChangedTiles(t *testing.T) {
	var out bytes.Buffer
	s := &screen{out: &out, fb: newFramebuffer(1000, 1000), rW: 500, rH: 500}
	s.buildMaps()
	s.patch([]Rect{{0, 0, 10, 10}, {980, 980, 10, 10}}) // two corners
	cmds := bytes.Count(out.Bytes(), []byte("a=f,i=1"))
	if cmds != 2 {
		t.Fatalf("%d redraws, want 2", cmds)
	}
	if !bytes.Contains(out.Bytes(), []byte("x=0,y=0,s=32,v=32")) || !bytes.Contains(out.Bytes(), []byte("x=480,y=480,s=20,v=20")) {
		t.Fatalf("tiles: %s", bytes.ReplaceAll(out.Bytes(), []byte{0x1b}, []byte("^")))
	}
	// A run of changed tiles along a row is one rectangle.
	out.Reset()
	for x := 0; x < 200; x++ { // pixels really change across 100 drawn pixels: tiles 0–3
		s.fb.put(x, 0, 9, 9, 9)
	}
	s.patch([]Rect{{0, 0, 200, 10}})
	if n := bytes.Count(out.Bytes(), []byte("a=f,i=1")); n != 1 || !bytes.Contains(out.Bytes(), []byte("x=0,y=0,s=128,v=32")) {
		t.Fatalf("run: %d redraws", n)
	}
}

// glance.lua must at least compile: Rex loads it from init.lua inside pcall,
// so a syntax error makes Glance at… vanish without a word.
func TestGlanceLuaCompiles(t *testing.T) {
	for _, tool := range [][]string{{"luajit", "-bl"}, {"luac", "-p"}} {
		if _, err := exec.LookPath(tool[0]); err != nil {
			continue
		}
		out, err := exec.Command(tool[0], append(tool[1:], "glance.lua")...).CombinedOutput()
		if err != nil {
			t.Fatalf("glance.lua doesn't compile:\n%s", firstLine(string(out)))
		}
		return
	}
	t.Skip("no Lua compiler to check glance.lua with")
}

// A server that says a big area changed when only a little did: only the
// tiles whose pixels really differ are redrawn.
func TestPatchSkipsTilesThatDidNotReallyChange(t *testing.T) {
	var out bytes.Buffer
	s := &screen{out: &out, fb: newFramebuffer(256, 256), rW: 256, rH: 256}
	s.buildMaps()
	s.full()
	out.Reset()
	s.fb.put(100, 100, 1, 2, 3)       // one pixel really changes…
	s.patch([]Rect{{0, 0, 256, 256}}) // …but the whole screen is reported
	if n := bytes.Count(out.Bytes(), []byte("a=f,i=1")); n != 1 || !bytes.Contains(out.Bytes(), []byte("x=96,y=96,s=32,v=32")) {
		t.Fatalf("%d redraws: %s", n, bytes.ReplaceAll(out.Bytes(), []byte{0x1b}, []byte("^")))
	}
	out.Reset()
	s.patch([]Rect{{0, 0, 256, 256}}) // nothing new since
	if out.Len() != 0 {
		t.Fatal("redrew tiles that hadn't changed")
	}
}
