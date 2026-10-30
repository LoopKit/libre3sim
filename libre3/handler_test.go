package libre3

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// memLink is a connection as a test client sees it: the sensor's notifies
// arrive on per-characteristic channels.
type memLink struct {
	mu     sync.Mutex
	notes  map[Char]chan []byte
	closed chan struct{}
	once   sync.Once
}

func newMemLink() *memLink {
	l := &memLink{notes: map[Char]chan []byte{}, closed: make(chan struct{})}
	for c := CharGlucose; c <= CharCommand; c++ {
		l.notes[c] = make(chan []byte, 1024)
	}
	return l
}

func (l *memLink) Addr() string                  { return "test" }
func (l *memLink) Notify(c Char, b []byte) error { l.notes[c] <- append([]byte(nil), b...); return nil }
func (l *memLink) Respond(c Char, b []byte)      { l.notes[c] <- append([]byte(nil), b...) }
func (l *memLink) Close()                        { l.once.Do(func() { close(l.closed) }) }

func (l *memLink) next(t *testing.T, c Char) []byte {
	t.Helper()
	select {
	case b := <-l.notes[c]:
		return b
	case <-l.closed:
		t.Fatalf("link closed waiting for %s", c)
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", c)
	}
	return nil
}

// message reassembles n bytes of sequence-numbered notifies.
func (l *memLink) message(t *testing.T, c Char, n int) []byte {
	t.Helper()
	var out []byte
	for len(out) < n {
		out = append(out, l.next(t, c)[1:]...)
	}
	return out
}

func (l *memLink) waitClosed(t *testing.T) {
	t.Helper()
	select {
	case <-l.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("link still open")
	}
}

// client is the app's side, following RoundWhiteDiscKit's PairingFlow.
type client struct {
	t    *testing.T
	h    Handler
	l    *memLink
	keys *Keys
}

func connect(t *testing.T, s *Sensor) *client {
	l := newMemLink()
	return &client{t: t, h: NewHandler(s, l), l: l, keys: s.keys}
}

func (c *client) command(cmd byte, want ...byte) {
	c.t.Helper()
	c.h.Write(CharCommand, []byte{cmd})
	if len(want) > 0 {
		if got := c.l.next(c.t, CharCommand); !bytes.Equal(got, want) {
			c.t.Fatalf("command %02x answered %x, want %x", cmd, got, want)
		}
	}
}

func (c *client) send(ch Char, msg []byte) {
	for off := 0; off < len(msg); off += writeChunk {
		end := off + writeChunk
		if end > len(msg) {
			end = len(msg)
		}
		c.h.Write(ch, append([]byte{byte(off), byte(off >> 8)}, msg[off:end]...))
	}
}

// fullHandshake runs the command-gated preamble and returns kAuth.
func (c *client) fullHandshake() []byte {
	t := c.t
	c.command(0x01)
	c.command(0x02)
	c.send(CharCert, c.keys.AppCertificate())
	c.command(0x03, 0x04)
	c.command(0x09, 0x0a)
	cert := c.l.message(t, CharCert, 140)
	if !c.keys.verify(cert[:76], cert[76:]) {
		t.Fatal("sensor certificate does not verify")
	}
	c.command(0x0d)
	phoneEph, _ := ecdh.P256().GenerateKey(rand.Reader)
	c.send(CharCert, append(phoneEph.PublicKey().Bytes(), make([]byte, 7)...))
	c.command(0x0e, 0x0f)
	sensorEph := c.l.message(t, CharCert, 65)
	ze, err := ecdhSecret(phoneEph.Bytes(), sensorEph)
	if err != nil {
		t.Fatal(err)
	}
	zs, err := ecdhSecret(mustHex(c.keys.AppKey), cert[11:76])
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(append(append([]byte{0, 0, 0, 1}, ze...), zs...))
	return h[:16]
}

// authorize runs StartAuthorization and Phase 5/6, returning the session
// keys, or nil when Phase 6 does not verify.
func (c *client) authorize(kAuth, pin []byte) *dataPlane {
	t := c.t
	c.command(0x11, 0x08)
	challenge := c.l.message(t, CharChallenge, 23)
	r1, nonce := challenge[:16], challenge[16:]
	r2 := random(16)
	p5 := ccmSeal(kAuth, nonce, append(append(append([]byte(nil), r1...), r2...), pin...))
	c.send(CharChallenge, append(p5, make([]byte, 14)...))
	c.command(0x08, 0x08)
	p6 := c.l.message(t, CharChallenge, 67)
	pt, err := ccmOpen(kAuth, p6[60:], p6[:60])
	if err != nil {
		return nil
	}
	if !bytes.Equal(pt[:16], r2) || !bytes.Equal(pt[16:32], r1) {
		t.Fatal("Phase 6 does not echo R2 and R1")
	}
	return newDataPlane(pt[32:48], pt[48:56])
}

func (c *client) arm() {
	for _, ch := range []Char{CharPatchControl, CharEventLog, CharFactory, CharGlucose, CharPatchStatus, CharHistoric, CharClinical} {
		c.h.Subscribed(ch, false)
		c.h.Subscribed(ch, true)
	}
}

func testSensor(t *testing.T, age time.Duration) *Sensor {
	dir := t.TempDir()
	keys, err := LoadOrCreateKeys(filepath.Join(dir, "keys.json"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := NewState("", 14, time.Hour, age)
	if err != nil {
		t.Fatal(err)
	}
	return NewSensor(st, keys, filepath.Join(dir, "state.json"))
}

func nfcParams(receiver uint32) []byte {
	p := make([]byte, 10)
	binary.LittleEndian.PutUint32(p, uint32(time.Now().Unix()))
	binary.LittleEndian.PutUint32(p[4:], receiver)
	binary.LittleEndian.PutUint16(p[8:], abbottCRC16(p[:8]))
	return p
}

func TestActivationPairingReconnectAndBackfill(t *testing.T) {
	s := testSensor(t, 3*time.Hour)
	receiver := uint32(0x6f0d8378)
	if _, err := s.NFC(0xa8, nfcParams(receiver), time.Now()); err != nil {
		t.Fatalf("claiming a worn sensor: %v", err)
	}
	if _, err := s.NFC(0xa8, nfcParams(receiver+1), time.Now()); err == nil {
		t.Fatal("switch receiver with another receiver ID succeeded")
	}
	info, _ := s.NFC(0xa1, nil, time.Now())
	if info[17] != nfcActive || string(info[18:27]) != s.state.Serial {
		t.Fatalf("patch info %x", info)
	}
	resp, err := s.NFC(0xa8, nfcParams(receiver), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(resp) != 19 || resp[1] != 0xa5 {
		t.Fatalf("activation response %x", resp)
	}
	pin := resp[9:13]

	c := connect(t, s)
	kAuth := c.fullHandshake()
	session := c.authorize(kAuth, pin)
	if session == nil {
		t.Fatal("Phase 6 does not verify after an NFC scan")
	}
	c.arm()
	status, err := session.open(CharPatchStatus, c.l.next(t, CharPatchStatus))
	if err != nil {
		t.Fatal(err)
	}
	if lc := binary.LittleEndian.Uint16(status[8:]); lc != 180 || status[7] != patchActive {
		t.Fatalf("patch status %x", status)
	}
	// The resubscribed clinical stream replays the post-warmup window.
	clinical, err := session.open(CharClinical, c.l.next(t, CharClinical))
	if err != nil || binary.LittleEndian.Uint16(clinical) != 60 {
		t.Fatalf("first clinical record %x, %v", clinical, err)
	}

	// Historical backfill from life count 100: pages of six 5-minute points.
	cmd := []byte{0x01, 0x00, 0x01, 100, 0, 0, 0}
	c.h.Write(CharPatchControl, (&dataPlane{kEnc: session.kEnc, ivEnc: session.ivEnc, seq: map[Char]uint16{}}).seal(CharPatchControl, cmd))
	page, err := session.open(CharHistoric, c.l.next(t, CharHistoric))
	if err != nil {
		t.Fatal(err)
	}
	if start := binary.LittleEndian.Uint16(page); start != 100 || binary.LittleEndian.Uint16(page[2:]) != s.state.reading(100) {
		t.Fatalf("first page %x", page)
	}
	c.h.Closed()

	// A cached reconnect: straight to StartAuthorization with the saved kAuth.
	c = connect(t, s)
	if c.authorize(kAuth, pin) == nil {
		t.Fatal("cached reconnect rejected")
	}
	c.h.Closed()

	// A full handshake without another NFC scan fails at Phase 6.
	c = connect(t, s)
	if c.authorize(c.fullHandshake(), pin) != nil {
		t.Fatal("full handshake accepted without an NFC scan")
	}
	c.h.Closed()

	// The PIN an NFC scan burned no longer authorizes.
	if _, err := s.NFC(0xa8, nfcParams(receiver), time.Now()); err != nil {
		t.Fatal(err)
	}
	c = connect(t, s)
	c.command(0x11, 0x08)
	challenge := c.l.message(t, CharChallenge, 23)
	p5 := ccmSeal(kAuth, challenge[16:], append(append(append([]byte(nil), challenge[:16]...), random(16)...), pin...))
	c.send(CharChallenge, append(p5, make([]byte, 14)...))
	c.h.Write(CharCommand, []byte{0x08})
	c.l.waitClosed(t)
}

func TestFreshSensorActivates(t *testing.T) {
	s := testSensor(t, 0)
	if s.Advertising(time.Now()) {
		t.Fatal("an unactivated sensor advertises")
	}
	if _, err := s.NFC(0xa8, nfcParams(1), time.Now()); err == nil {
		t.Fatal("switch receiver on an unactivated sensor succeeded")
	}
	info, _ := s.NFC(0xa1, nil, time.Now())
	if info[17] != nfcNotActivated {
		t.Fatalf("state %02x", info[17])
	}
	if _, err := s.NFC(0xa0, nfcParams(42), time.Now()); err != nil {
		t.Fatal(err)
	}
	if !s.Advertising(time.Now()) || s.state.ReceiverID != 42 {
		t.Fatal("activation did not take")
	}
	info, _ = s.NFC(0xa1, nil, time.Now())
	if info[17] != nfcWarmup {
		t.Fatalf("state after activation %02x", info[17])
	}
}

func TestUntrustedCertificateDropsTheLink(t *testing.T) {
	s := testSensor(t, time.Hour)
	other, _ := LoadOrCreateKeys(filepath.Join(t.TempDir(), "other.json"))
	c := connect(t, s)
	c.command(0x01)
	c.command(0x02)
	c.send(CharCert, other.AppCertificate())
	c.h.Write(CharCommand, []byte{0x03})
	c.l.waitClosed(t)
}
