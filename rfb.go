package main

// The RFB (VNC) protocol, as much as rex-glance needs: version 3.8, Apple
// Remote Desktop's Diffie-Hellman login (security type 30, which macOS Screen
// Sharing requires and wayvnc offers), 32-bit true colour, and framebuffer
// updates. Message layouts follow RFC 6143; the Apple login follows the
// public description implemented by libvncclient and rootshell-vnc.

import (
	"bufio"
	"crypto/aes"
	"crypto/md5"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"
	"sync"
)

// Encodings rex-glance asks for, best first for latency on a LAN or tailnet.
const (
	encRaw         int32 = 0
	encCopyRect    int32 = 1
	encZlib        int32 = 6
	encZRLE        int32 = 16
	encDesktopSize int32 = -223
)

type rfbConn struct {
	wmu    sync.Mutex // input and update requests write from different goroutines
	rw     io.ReadWriteCloser
	r      *bufio.Reader
	Width  int
	Height int
	Name   string

	zrle *inflater // ZRLE's zlib stream, which runs across every rectangle
	zlib *inflater // the Zlib encoding's, a separate stream
}

// send writes one whole client message.
func (c *rfbConn) send(msg []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err := c.rw.Write(msg)
	return err
}

func (c *rfbConn) read(n int) ([]byte, error) {
	b := make([]byte, n)
	_, err := io.ReadFull(c.r, b)
	return b, err
}

func (c *rfbConn) u8() (byte, error) { return c.r.ReadByte() }

func (c *rfbConn) u16() (int, error) {
	b, err := c.read(2)
	if err != nil {
		return 0, err
	}
	return int(binary.BigEndian.Uint16(b)), nil
}

func (c *rfbConn) u32() (uint32, error) {
	b, err := c.read(4)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(b), nil
}

// handshake runs the opening of a session: version, login, and init. It
// leaves the connection set to 32-bit true colour with our encodings.
func handshake(rw io.ReadWriteCloser, user, password string) (*rfbConn, error) {
	c := &rfbConn{rw: rw, r: bufio.NewReaderSize(rw, 1<<16), zrle: &inflater{}, zlib: &inflater{}}

	banner, err := c.read(12)
	if err != nil {
		return nil, fmt.Errorf("no RFB greeting: %w", err)
	}
	if !strings.HasPrefix(string(banner), "RFB ") {
		return nil, fmt.Errorf("not a VNC server (it said %q)", banner)
	}
	if _, err := rw.Write([]byte("RFB 003.008\n")); err != nil {
		return nil, err
	}

	n, err := c.u8()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		reason, _ := c.reason()
		return nil, fmt.Errorf("the server refused the connection: %s", reason)
	}
	types, err := c.read(int(n))
	if err != nil {
		return nil, err
	}
	switch {
	case hasByte(types, 30):
		if _, err := rw.Write([]byte{30}); err != nil {
			return nil, err
		}
		if err := c.ardLogin(user, password); err != nil {
			return nil, err
		}
	case hasByte(types, 1) && password == "":
		if _, err := rw.Write([]byte{1}); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("the server offers no login rex-glance speaks (types %v; it needs 30, Apple Remote Desktop)", types)
	}

	result, err := c.u32()
	if err != nil {
		return nil, err
	}
	if result != 0 {
		reason, _ := c.reason()
		if reason == "" {
			reason = "wrong username or password?"
		}
		return nil, fmt.Errorf("login refused: %s", reason)
	}

	// ClientInit: shared, so we don't throw anyone else off.
	if _, err := rw.Write([]byte{1}); err != nil {
		return nil, err
	}
	if c.Width, err = c.u16(); err != nil {
		return nil, err
	}
	if c.Height, err = c.u16(); err != nil {
		return nil, err
	}
	if _, err := c.read(16); err != nil { // the server's pixel format; we set our own
		return nil, err
	}
	nameLen, err := c.u32()
	if err != nil {
		return nil, err
	}
	if nameLen > 1<<16 {
		return nil, errors.New("server name too long")
	}
	name, err := c.read(int(nameLen))
	if err != nil {
		return nil, err
	}
	c.Name = string(name)

	if err := c.setPixelFormat(); err != nil {
		return nil, err
	}
	if err := c.setEncodings(encZRLE, encZlib, encCopyRect, encRaw, encDesktopSize); err != nil {
		return nil, err
	}
	return c, nil
}

func hasByte(b []byte, v byte) bool {
	for _, x := range b {
		if x == v {
			return true
		}
	}
	return false
}

func (c *rfbConn) reason() (string, error) {
	n, err := c.u32()
	if err != nil || n > 1<<16 {
		return "", err
	}
	b, err := c.read(int(n))
	return string(b), err
}

// ardLogin is Apple Remote Desktop authentication (type 30): a Diffie-Hellman
// exchange on the server's group, whose shared secret, hashed with MD5, is
// the AES-128 key for the username and password, each in a 64-byte field.
func (c *rfbConn) ardLogin(user, password string) error {
	gen, err := c.u16()
	if err != nil {
		return err
	}
	keyLen, err := c.u16()
	if err != nil {
		return err
	}
	if keyLen == 0 || keyLen > 1024 {
		return fmt.Errorf("unexpected Apple login key length %d", keyLen)
	}
	primeB, err := c.read(keyLen)
	if err != nil {
		return err
	}
	peerB, err := c.read(keyLen)
	if err != nil {
		return err
	}
	msg, pub, err := ardResponse(gen, primeB, peerB, user, password)
	if err != nil {
		return err
	}
	_, err = c.rw.Write(append(msg, pub...))
	return err
}

// ardResponse computes the client's half of the Apple login: the encrypted
// credentials and the client's public key, both as sent on the wire.
func ardResponse(gen int, primeB, peerB []byte, user, password string) (cipherText, publicKey []byte, err error) {
	if len(user) > 63 || len(password) > 63 {
		return nil, nil, errors.New("username and password must each be under 64 bytes")
	}
	keyLen := len(primeB)
	p := new(big.Int).SetBytes(primeB)
	g := big.NewInt(int64(gen))
	peer := new(big.Int).SetBytes(peerB)

	priv, err := rand.Int(rand.Reader, new(big.Int).Sub(p, big.NewInt(2)))
	if err != nil {
		return nil, nil, err
	}
	priv.Add(priv, big.NewInt(1))
	pub := new(big.Int).Exp(g, priv, p)
	secret := new(big.Int).Exp(peer, priv, p)

	key := md5.Sum(leftPad(secret.Bytes(), keyLen))

	creds := make([]byte, 128)
	if _, err := rand.Read(creds); err != nil {
		return nil, nil, err
	}
	copy(creds[0:], user)
	creds[len(user)] = 0
	copy(creds[64:], password)
	creds[64+len(password)] = 0

	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, nil, err
	}
	out := make([]byte, 128)
	for i := 0; i < 128; i += aes.BlockSize { // ECB, as Apple's scheme specifies
		block.Encrypt(out[i:i+aes.BlockSize], creds[i:i+aes.BlockSize])
	}
	return out, leftPad(pub.Bytes(), keyLen), nil
}

func leftPad(b []byte, n int) []byte {
	if len(b) >= n {
		return b
	}
	out := make([]byte, n)
	copy(out[n-len(b):], b)
	return out
}

// setPixelFormat asks for 32-bit little-endian true colour, red in bits
// 16-23: each pixel arrives as B, G, R, unused.
func (c *rfbConn) setPixelFormat() error {
	msg := []byte{0, 0, 0, 0,
		32, 24, 0, 1, // bits per pixel, depth, big endian, true colour
		0, 255, 0, 255, 0, 255, // red, green, blue max
		16, 8, 0, // shifts
		0, 0, 0}
	return c.send(msg)
}

func (c *rfbConn) setEncodings(encs ...int32) error {
	msg := []byte{2, 0, 0, byte(len(encs))}
	for _, e := range encs {
		msg = binary.BigEndian.AppendUint32(msg, uint32(e))
	}
	return c.send(msg)
}

// request asks for an update of the whole screen; incremental asks only for
// what changed since the last one.
func (c *rfbConn) request(incremental bool) error {
	inc := byte(0)
	if incremental {
		inc = 1
	}
	msg := []byte{3, inc, 0, 0, 0, 0}
	msg = binary.BigEndian.AppendUint16(msg, uint16(c.Width))
	msg = binary.BigEndian.AppendUint16(msg, uint16(c.Height))
	return c.send(msg)
}

// pointer sends the mouse position and buttons (bit 0 left, 1 middle, 2
// right, 3 and 4 the wheel).
func (c *rfbConn) pointer(buttons byte, x, y int) error {
	msg := []byte{5, buttons}
	msg = binary.BigEndian.AppendUint16(msg, uint16(clamp(x, 0, c.Width-1)))
	msg = binary.BigEndian.AppendUint16(msg, uint16(clamp(y, 0, c.Height-1)))
	return c.send(msg)
}

// key sends a key press or release, as an X keysym.
func (c *rfbConn) key(down bool, keysym uint32) error {
	d := byte(0)
	if down {
		d = 1
	}
	msg := []byte{4, d, 0, 0}
	msg = binary.BigEndian.AppendUint32(msg, keysym)
	return c.send(msg)
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// Rect is a changed area of the screen, in framebuffer pixels.
type Rect struct{ X, Y, W, H int }

// next reads one server message. It applies a framebuffer update to fb and
// returns the rectangles that changed; resized is set when the screen
// changed size (fb has been reallocated).
func (c *rfbConn) next(fb *framebuffer) (changed []Rect, resized bool, err error) {
	t, err := c.u8()
	if err != nil {
		return nil, false, err
	}
	switch t {
	case 0: // FramebufferUpdate
		if _, err := c.u8(); err != nil {
			return nil, false, err
		}
		n, err := c.u16()
		if err != nil {
			return nil, false, err
		}
		for i := 0; i < n; i++ {
			h, err := c.read(12)
			if err != nil {
				return nil, false, err
			}
			r := Rect{int(binary.BigEndian.Uint16(h[0:])), int(binary.BigEndian.Uint16(h[2:])),
				int(binary.BigEndian.Uint16(h[4:])), int(binary.BigEndian.Uint16(h[6:]))}
			enc := int32(binary.BigEndian.Uint32(h[8:]))
			if enc == encDesktopSize {
				c.Width, c.Height = r.W, r.H
				fb.resize(r.W, r.H)
				resized = true
				continue
			}
			if err := c.decode(fb, r, enc); err != nil {
				return nil, false, err
			}
			changed = append(changed, r)
		}
		return changed, resized, nil
	case 1: // SetColourMapEntries: not used with true colour; skip it
		b, err := c.read(5)
		if err != nil {
			return nil, false, err
		}
		_, err = c.read(int(binary.BigEndian.Uint16(b[3:])) * 6)
		return nil, false, err
	case 2: // Bell
		return nil, false, nil
	case 3: // ServerCutText
		if _, err := c.read(3); err != nil {
			return nil, false, err
		}
		n, err := c.u32()
		if err != nil {
			return nil, false, err
		}
		if n&0x80000000 != 0 { // extended clipboard: a length with the top bit set
			n = -n
		}
		_, err = c.read(int(n))
		return nil, false, err
	}
	return nil, false, fmt.Errorf("unknown server message type %d", t)
}
