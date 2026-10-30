package libre3

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"log"
	"sync"
	"time"
)

// The sensor's side of one connection. The wire protocol it answers is the
// client in RoundWhiteDiscKit: PairingFlow (the command-gated handshake and the
// cached reconnect), SensorSession (fragmentation, CCCD arming) and
// LibreLoop's sensor monitor (data plane and backfill requests).
//
// The security command characteristic clocks the handshake; each command is
// one byte, and the sensor answers on the same characteristic:
//
//	01 StartAuthentication       a full handshake begins
//	02 LoadCertificate           the phone writes its 162-byte certificate
//	03 SendCertificateLoadDone   -> 04, or the link drops if the cert is not trusted
//	09 GetCertificate            -> 0a, then the 140-byte sensor certificate
//	0d ValidateCertificate       the phone writes its ephemeral key (72 bytes)
//	0e SendEphemeralDone         -> 0f, then the sensor's ephemeral key
//	11 StartAuthorization        -> 08, then R1 || nonce7 (cached reconnects start here)
//	08 SendChallengeLoadDone     the phone wrote Phase 5 -> 08, then Phase 6

// idleTimeout is how long the sensor waits for an unauthorized link to
// finish its handshake before hanging up.
const idleTimeout = 60 * time.Second

type handlerEvent struct {
	kind    int // eventWrite, eventSubscribe, eventRead
	char    Char
	data    []byte
	enabled bool
}

const (
	eventWrite = iota
	eventSubscribe
	eventRead
)

type handler struct {
	sensor *Sensor
	link   Link
	events chan handlerEvent
	done   chan struct{}
	once   sync.Once

	// The handshake in progress.
	full      bool // StartAuthentication: certificates and ephemerals this time
	certBuf   writeBuffer
	ephBuf    writeBuffer
	chalBuf   writeBuffer
	toEph     bool // cert writes are now the ephemeral key
	phoneCert []byte
	key       []byte // kAuth for Phase 5/6
	r1, nonce []byte

	// After Phase 6.
	session   *dataPlane
	armed     map[Char]bool // CCCDs enabled since authorization
	streaming bool
	lastLC    int
}

// NewHandler builds the handler for a new link and starts its goroutine.
func NewHandler(sensor *Sensor, link Link) Handler {
	h := &handler{sensor: sensor, link: link, events: make(chan handlerEvent, 256),
		done: make(chan struct{}), armed: map[Char]bool{}}
	go h.run()
	return h
}

func (h *handler) enqueue(e handlerEvent) {
	select {
	case h.events <- e:
	case <-h.done:
	default:
		log.Printf("%s: event queue full; dropping %s", h.link.Addr(), e.char)
	}
}

func (h *handler) Write(c Char, data []byte) {
	h.enqueue(handlerEvent{kind: eventWrite, char: c, data: append([]byte(nil), data...)})
}

func (h *handler) Subscribed(c Char, enabled bool) {
	h.enqueue(handlerEvent{kind: eventSubscribe, char: c, enabled: enabled})
}

func (h *handler) Read(c Char) { h.enqueue(handlerEvent{kind: eventRead, char: c}) }

func (h *handler) Closed() { h.once.Do(func() { close(h.done) }) }

func (h *handler) run() {
	idle := time.NewTimer(idleTimeout)
	defer idle.Stop()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-h.done:
			return
		case e := <-h.events:
			switch e.kind {
			case eventWrite:
				h.write(e.char, e.data)
			case eventSubscribe:
				h.subscribed(e.char, e.enabled)
			case eventRead:
				h.read(e.char)
			}
		case <-idle.C:
			if h.session == nil {
				log.Printf("%s: no authorization in %v; hanging up", h.link.Addr(), idleTimeout)
				h.hangUp()
			}
		case now := <-tick.C:
			h.minute(now)
		}
	}
}

func (h *handler) hangUp() {
	h.link.Close()
	h.Closed()
}

func (h *handler) notify(c Char, b []byte) {
	if err := h.link.Notify(c, b); err != nil {
		log.Printf("%s: %s notify dropped: %v", h.link.Addr(), c, err)
	}
}

func (h *handler) notifyMessage(c Char, msg []byte) {
	for _, f := range notifyFragments(msg) {
		h.notify(c, f)
	}
}

func (h *handler) write(c Char, data []byte) {
	switch c {
	case CharCommand:
		if len(data) != 1 {
			log.Printf("%s: command write of %d bytes ignored", h.link.Addr(), len(data))
			return
		}
		h.command(data[0])
	case CharCert:
		buf := &h.certBuf
		if h.toEph {
			buf = &h.ephBuf
		}
		if err := buf.feed(data); err != nil {
			log.Printf("%s: cert write: %v", h.link.Addr(), err)
		}
	case CharChallenge:
		if err := h.chalBuf.feed(data); err != nil {
			log.Printf("%s: challenge write: %v", h.link.Addr(), err)
		}
	case CharPatchControl:
		h.patchControl(data)
	default:
		log.Printf("%s: write to %s ignored: %s", h.link.Addr(), c, hex.EncodeToString(data))
	}
}

func (h *handler) command(cmd byte) {
	addr := h.link.Addr()
	switch cmd {
	case 0x01:
		log.Printf("%s: StartAuthentication", addr)
		h.full, h.toEph, h.key, h.session, h.streaming = true, false, nil, nil, false
		h.armed = map[Char]bool{}
	case 0x02:
		h.certBuf = writeBuffer{}
	case 0x03:
		h.phoneCert = append([]byte(nil), h.certBuf.data...)
		if !h.sensor.keys.acceptsPhoneCert(h.phoneCert) {
			log.Printf("%s: phone certificate (%d bytes) not trusted; dropping the link", addr, len(h.phoneCert))
			h.hangUp()
			return
		}
		h.notify(CharCommand, []byte{0x04})
	case 0x09:
		h.notify(CharCommand, []byte{0x0a})
		h.notifyMessage(CharCert, h.sensor.certificate())
	case 0x0d:
		h.toEph, h.ephBuf = true, writeBuffer{}
	case 0x0e:
		if len(h.ephBuf.data) < 65 || len(h.phoneCert) != 162 {
			log.Printf("%s: SendEphemeralDone without a certificate and ephemeral key; dropping the link", addr)
			h.hangUp()
			return
		}
		eph := randomScalar()
		key, err := authKey(eph, h.ephBuf.data[:65], h.sensor.staticKey(), h.phoneCert[33:98])
		if err != nil {
			log.Printf("%s: key agreement: %v; dropping the link", addr, err)
			h.hangUp()
			return
		}
		h.key = key
		h.notify(CharCommand, []byte{0x0f})
		h.notifyMessage(CharCert, publicPoint(eph))
	case 0x11:
		if !h.full {
			h.key = h.sensor.cachedAuthKey()
			log.Printf("%s: StartAuthorization (cached reconnect, %s)", addr, map[bool]string{true: "key on file", false: "no key on file"}[h.key != nil])
		}
		h.r1, h.nonce = random(16), random(7)
		h.chalBuf = writeBuffer{}
		h.notify(CharCommand, []byte{0x08})
		h.notifyMessage(CharChallenge, append(append([]byte(nil), h.r1...), h.nonce...))
	case 0x08:
		h.phase6()
	default:
		log.Printf("%s: unknown security command %02x", addr, cmd)
	}
}

// phase6 checks the phone's Phase 5 (R1 || R2 || PIN under kAuth) and answers
// with the session keys. A full handshake on a sensor no NFC scan has opened
// is answered under a key the phone does not have, as a real active sensor
// fails it at Phase 6.
func (h *handler) phase6() {
	addr := h.link.Addr()
	p5 := h.chalBuf.data
	var pt []byte
	err := errAuth
	if h.key != nil && h.nonce != nil && len(p5) >= 40 {
		pt, err = ccmOpen(h.key, h.nonce, p5[:40])
	}
	if err != nil {
		log.Printf("%s: Phase 5 does not verify; dropping the link", addr)
		h.hangUp()
		return
	}
	if !bytes.Equal(pt[:16], h.r1) || !bytes.Equal(pt[32:36], h.sensor.pin()) {
		log.Printf("%s: Phase 5 has the wrong R1 or BLE PIN; dropping the link", addr)
		h.hangUp()
		return
	}
	key := h.key
	accepted := !h.full || h.sensor.pairingOpen()
	if !accepted {
		log.Printf("%s: full handshake without an NFC scan since the last one; answering Phase 6 under the wrong key", addr)
		key = random(16)
	}
	kEnc := random(16)
	ivEnc := append([]byte{0, 0, 0, 0}, random(4)...)
	nonce := random(7)
	plain := append(append(append(append([]byte(nil), pt[16:32]...), h.r1...), kEnc...), ivEnc...)
	h.notify(CharCommand, []byte{0x08})
	h.notifyMessage(CharChallenge, append(ccmSeal(key, nonce, plain), nonce...))
	if !accepted {
		return
	}
	if h.full {
		h.sensor.authorized(h.key)
		log.Printf("%s: authorized (full handshake)", addr)
	} else {
		log.Printf("%s: authorized (cached reconnect)", addr)
	}
	h.session = newDataPlane(kEnc, ivEnc)
	h.armed = map[Char]bool{}
	h.streaming = false
}

func (h *handler) subscribed(c Char, enabled bool) {
	if h.session == nil {
		return
	}
	if !enabled {
		delete(h.armed, c)
		return
	}
	h.armed[c] = true
	if c == CharClinical && h.streaming {
		h.replayClinical()
	}
	if !h.streaming && h.armed[CharGlucose] && h.armed[CharPatchStatus] {
		h.streaming = true
		h.lastLC = h.sensor.Tick(time.Now())
		log.Printf("%s: data plane armed; streaming from life count %d", h.link.Addr(), h.lastLC)
		h.sendPatchStatus()
		if h.armed[CharClinical] {
			h.replayClinical()
		}
	}
}

// minute sends what a connected sensor sends each minute: patch status, the
// realtime reading and, while subscribed, the clinical record.
func (h *handler) minute(now time.Time) {
	if !h.sensor.InRange(now) {
		log.Printf("%s: out of range; the link drops", h.link.Addr())
		h.hangUp()
		return
	}
	if !h.streaming {
		return
	}
	lc := h.sensor.Tick(now)
	if lc <= h.lastLC {
		return
	}
	h.lastLC = lc
	h.sendPatchStatus()
	if r, ok := h.sensor.realtime(lc); ok {
		frame := h.session.seal(CharGlucose, r.bytes())
		h.notify(CharGlucose, frame[:15])
		h.notify(CharGlucose, frame[15:])
		log.Printf("%s: glucose %d mg/dL at life count %d", h.link.Addr(), r.Glucose, lc)
	}
	if h.armed[CharClinical] {
		h.sendClinical(lc, lc)
	}
}

func (h *handler) sendPatchStatus() {
	if !h.armed[CharPatchStatus] {
		return
	}
	h.notify(CharPatchStatus, h.session.seal(CharPatchStatus, h.sensor.patchStatus(time.Now()).bytes()))
}

// The clinical stream buffers while no one is subscribed and replays the
// missed window on the next subscription, up to six hours of it.
func (h *handler) replayClinical() {
	lc := h.sensor.Tick(time.Now())
	from := h.sensor.clinicalSent() + 1
	if from < lc-360 {
		from = lc - 360
	}
	h.sendClinical(from, lc)
}

func (h *handler) sendClinical(from, to int) {
	records := h.sensor.clinicalRecords(from, to)
	for _, r := range records {
		h.notify(CharClinical, h.session.seal(CharClinical, r))
	}
	if len(records) > 0 {
		h.sensor.setClinicalSent(to)
	}
}

func (h *handler) read(c Char) {
	if c != CharPatchStatus || h.session == nil {
		h.link.Respond(c, nil)
		return
	}
	h.link.Respond(c, h.session.seal(CharPatchStatus, h.sensor.patchStatus(time.Now()).bytes()))
}

func (h *handler) patchControl(frame []byte) {
	addr := h.link.Addr()
	if h.session == nil {
		log.Printf("%s: patchControl write before authorization ignored", addr)
		return
	}
	cmd, err := h.session.open(CharPatchControl, frame)
	if err != nil || len(cmd) != 7 {
		log.Printf("%s: patchControl write does not decrypt: %s", addr, hex.EncodeToString(frame))
		return
	}
	now := time.Now()
	switch {
	case cmd[0] == 0x01 && cmd[1] == 0x00:
		from := int(binary.LittleEndian.Uint16(cmd[3:]))
		pages := h.sensor.historicPages(from, now)
		log.Printf("%s: historical backfill >= %d: %d pages", addr, from, len(pages))
		for _, p := range pages {
			h.notify(CharHistoric, h.session.seal(CharHistoric, p))
		}
	case cmd[0] == 0x01 && cmd[1] == 0x01:
		from := int(binary.LittleEndian.Uint16(cmd[3:]))
		log.Printf("%s: clinical backfill >= %d", addr, from)
		h.sendClinical(from, h.sensor.Tick(now))
	case cmd[0] == 0x05:
		log.Printf("%s: shutdown requested", addr)
		h.sensor.Terminate(errorTerminated, now)
		h.sendPatchStatus()
	default:
		log.Printf("%s: patchControl %s not modelled", addr, hex.EncodeToString(cmd))
	}
}
