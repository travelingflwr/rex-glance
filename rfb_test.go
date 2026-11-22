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
	if c.Width != 8 || c.Height != 4 || c.Name != "fake" {
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
	s := &screen{fb: fb, imgW: 2, imgH: 1}
	ox, oy, w, h, pix := s.sample(Rect{2, 0, 2, 2})
	if ox != 1 || oy != 0 || w != 1 || h != 1 || len(pix) != 3 || pix[0] != fb.Pix[2*3] {
		t.Fatalf("%d,%d %dx%d %v", ox, oy, w, h, pix)
	}
}
