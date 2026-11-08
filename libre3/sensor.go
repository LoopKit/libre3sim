package libre3

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// Sensor is the emulated Libre 3: its persisted state plus what is only true
// while the emulator runs (whether a phone is connected, a staged outage).
type Sensor struct {
	keys *Keys
	path string

	mu        sync.Mutex
	state     *State
	connected bool
	outUntil  time.Time // out of range: no advertising, links drop
	silenced  bool      // a terminated sensor whose last link has closed
	lastSaved int
	retired   bool
}

func NewSensor(state *State, keys *Keys, path string) *Sensor {
	s := &Sensor{state: state, keys: keys, path: path, lastSaved: -1}
	if state.Terminated != nil {
		s.silenced = true
	}
	return s
}

func (s *Sensor) Keys() *Keys { return s.keys }

func (s *Sensor) Serial() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.Serial
}

func (s *Sensor) AdvertisedName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.AdvertisedName()
}

func (s *Sensor) save() {
	if s.retired {
		return
	}
	if err := s.state.Save(s.path); err != nil {
		log.Printf("saving %s: %v", s.path, err)
	}
}

// Retire stops the sensor writing its state file, before another replaces it.
func (s *Sensor) Retire() {
	s.mu.Lock()
	s.retired = true
	s.mu.Unlock()
}

// Tick advances the sensor's clock: readings are made up minute by minute
// and saved. It returns the life count.
func (s *Sensor) Tick(now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	lc := s.state.lifeCount(now)
	if s.state.Activated != nil {
		s.state.fill(lc)
	}
	if lc != s.lastSaved {
		s.lastSaved = lc
		s.save()
	}
	return lc
}

// Advertising is whether a phone can find and connect to the sensor now: it
// is activated, in range, and not shut down. A Libre 3 holds one link and
// does not advertise while connected.
func (s *Sensor) Advertising(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reachable(now) && !s.connected
}

func (s *Sensor) reachable(now time.Time) bool {
	return s.state.Activated != nil && !s.silenced && !now.Before(s.outUntil)
}

// InRange is whether a live link survives: false during a staged outage.
func (s *Sensor) InRange(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !now.Before(s.outUntil)
}

func (s *Sensor) LinkOpened() {
	s.mu.Lock()
	s.connected = true
	s.mu.Unlock()
}

func (s *Sensor) LinkClosed() {
	s.mu.Lock()
	s.connected = false
	if s.state.Terminated != nil && !s.silenced {
		s.silenced = true
		log.Printf("sensor is shut down; no longer advertising")
	}
	s.mu.Unlock()
}

// GoOutOfRange stages an outage: the link drops and the sensor cannot be
// reached for d.
func (s *Sensor) GoOutOfRange(d time.Duration) {
	s.mu.Lock()
	s.outUntil = time.Now().Add(d)
	s.mu.Unlock()
}

// Terminate ends the sensor early with errorData (6 terminated, 8 replace
// sensor, 3 insertion failure), reported from now on.
func (s *Sensor) Terminate(errorData uint16, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := uint8(patchTerminated)
	if errorData == 8 {
		state = 8
	} else if errorData == 3 || errorData == 7 {
		state = uint8(errorData)
	}
	s.state.Terminated = &Termination{LifeCount: uint16(s.state.lifeCount(now)), ErrorData: errorData, PatchState: state}
	if !s.connected {
		s.silenced = true
	}
	s.save()
}

// LogEvent records an event-log entry with errorData while the sensor keeps
// working: patch status reports it with patch state 4 until the next event.
func (s *Sensor) LogEvent(errorData uint16, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.LastEvent = &Termination{LifeCount: uint16(s.state.lifeCount(now)), ErrorData: errorData, PatchState: patchActive}
	s.save()
}

func (s *Sensor) SetGlucoseMode(m GlucoseMode) {
	s.mu.Lock()
	s.state.Glucose = m
	s.save()
	s.mu.Unlock()
}

// NFCError is a sensor's error reply to an NFC command. Codes 0xb0 and up are the
// sensor's own rejections, which it sends as the response a5 01 <code>.
type NFCError struct {
	Code    byte
	Message string
}

func (e *NFCError) Error() string { return fmt.Sprintf("NFC error 0x%02x: %s", e.Code, e.Message) }

// NFC answers an ISO 15693 custom command (manufacturer code 0x7a) the way
// CoreNFC returns it to RoundWhiteDiscKit.
func (s *Sensor) NFC(code byte, params []byte, now time.Time) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch code {
	case 0xa1:
		return s.patchInfo(now), nil
	case 0xa0, 0xa8:
		if len(params) != 10 {
			return nil, &NFCError{0x02, fmt.Sprintf("want 10 bytes of parameters, got %d", len(params))}
		}
		if got, want := binary.LittleEndian.Uint16(params[8:]), abbottCRC16(params[:8]); got != want {
			return nil, &NFCError{0x02, fmt.Sprintf("bad CRC %04x, want %04x", got, want)}
		}
		receiver := binary.LittleEndian.Uint32(params[4:])
		activated := s.state.Activated != nil
		switch {
		case code == 0xa0 && activated:
			return nil, &NFCError{0xb0, "already activated; switch receiver instead"}
		case code == 0xa8 && !activated:
			return nil, &NFCError{0xb0, "not activated yet"}
		case code == 0xa8 && s.state.Terminated != nil:
			return nil, &NFCError{0xb0, "terminated"}
		case code == 0xa8 && s.state.ReceiverID != 0 && receiver != s.state.ReceiverID:
			return nil, &NFCError{0xb1, fmt.Sprintf("receiver %08x is not the one this sensor was activated with", receiver)}
		}
		if code == 0xa0 {
			at := now.Truncate(time.Second)
			s.state.Activated = &at
			s.state.ReceiverID = receiver
			s.lastSaved = -1
			log.Printf("NFC: activated for receiver %08x", receiver)
		} else {
			if s.state.ReceiverID == 0 {
				s.state.ReceiverID = receiver
			}
			log.Printf("NFC: switched to receiver %08x", receiver)
		}
		// Each activation or switch burns the BLE PIN and opens the sensor
		// to one full handshake.
		s.state.PIN = hex.EncodeToString(random(4))
		s.state.PairingOpen = true
		s.save()
		return s.activationResponse(), nil
	default:
		return nil, &NFCError{0x01, fmt.Sprintf("unknown command 0x%02x", code)}
	}
}

func (s *Sensor) nfcState(now time.Time) byte {
	st := s.state
	lc := st.lifeCount(now)
	switch {
	case st.Activated == nil:
		return nfcNotActivated
	case st.Terminated != nil:
		return nfcTerminated
	case lc >= st.WearMinutes:
		return nfcExpired
	default:
		return nfcActive
	}
}

func (s *Sensor) patchInfo(now time.Time) []byte {
	st := s.state
	b := make([]byte, 29)
	b[1] = 0xa5
	binary.LittleEndian.PutUint16(b[3:], st.SecurityVersion)
	binary.LittleEndian.PutUint16(b[5:], st.Region)
	binary.LittleEndian.PutUint16(b[7:], st.Generation)
	binary.LittleEndian.PutUint16(b[9:], uint16(st.WearMinutes))
	copy(b[11:15], st.Firmware[:])
	b[15] = st.ProductType
	b[16] = byte(st.WarmupMinutes / 5)
	b[17] = s.nfcState(now)
	copy(b[18:27], st.Serial)
	binary.LittleEndian.PutUint16(b[27:], abbottCRC16(b[2:27]))
	return b
}

func (s *Sensor) activationResponse() []byte {
	b := []byte{0x00, 0xa5, 0x00}
	addr := mustHex(strings.ReplaceAll(s.state.BLEAddress, ":", ""))
	for i := len(addr) - 1; i >= 0; i-- {
		b = append(b, addr[i])
	}
	b = append(b, mustHex(s.state.PIN)...)
	t := make([]byte, 4)
	binary.LittleEndian.PutUint32(t, uint32(s.state.Activated.Unix()))
	b = append(b, t...)
	crc := abbottCRC16(b[3:])
	return append(b, byte(crc), byte(crc>>8))
}

// abbottCRC16 is CRC-CCITT (0x1021, init 0xffff) over bit-reversed bytes,
// as RoundWhiteDiscKit's NFCActivationCommand computes it.
func abbottCRC16(data []byte) uint16 {
	crc := uint16(0xffff)
	for _, b := range data {
		var r byte
		for i := 0; i < 8; i++ {
			r = r<<1 | (b>>uint(i))&1
		}
		crc ^= uint16(r) << 8
		for i := 0; i < 8; i++ {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// What the handshake needs from the sensor.

func (s *Sensor) staticKey() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return mustHex(s.state.StaticKey)
}

func (s *Sensor) certificate() []byte {
	s.mu.Lock()
	serial, key := s.state.Serial, s.state.StaticKey
	s.mu.Unlock()
	return s.keys.sensorCertificate(serial, publicPoint(mustHex(key)))
}

func (s *Sensor) pin() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return mustHex(s.state.PIN)
}

func (s *Sensor) pairingOpen() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.PairingOpen
}

func (s *Sensor) cachedAuthKey() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.AuthKey == "" {
		return nil
	}
	return mustHex(s.state.AuthKey)
}

// authorized records a completed full handshake: its kAuth now serves cached
// reconnects, and another full handshake needs another NFC scan.
func (s *Sensor) authorized(key []byte) {
	s.mu.Lock()
	s.state.AuthKey = hex.EncodeToString(key)
	s.state.PairingOpen = false
	s.save()
	s.mu.Unlock()
}

// What the data plane needs.

func (s *Sensor) patchStatus(now time.Time) PatchStatusRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state
	lc := uint16(st.lifeCount(now))
	p := PatchStatusRecord{LifeCount: lc, PatchState: patchActive, CurrentLifeCount: lc}
	switch {
	case st.Terminated != nil:
		p.LifeCount, p.ErrorData, p.PatchState = st.Terminated.LifeCount, st.Terminated.ErrorData, st.Terminated.PatchState
	case int(lc) >= st.WearMinutes:
		p.LifeCount, p.ErrorData, p.PatchState = uint16(st.WearMinutes), errorExpired, patchExpired
	case st.LastEvent != nil:
		p.LifeCount, p.ErrorData = st.LastEvent.LifeCount, st.LastEvent.ErrorData
	}
	return p
}

// committedHistoric is the latest five-minute point the sensor has committed
// at life count lc: lc - 17, rounded down to a multiple of five.
func committedHistoric(lc int) int {
	h := lc - 17
	if h < 0 {
		return -1
	}
	return h - h%5
}

// realtime is the reading for life count lc, or false while the sensor has
// none to give (warmup, ended).
func (s *Sensor) realtime(lc int) (Realtime, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state
	if st.Activated == nil || st.Terminated != nil || lc < st.WarmupMinutes || lc >= st.WearMinutes {
		return Realtime{}, false
	}
	g := st.reading(lc)
	if g == 0 {
		return Realtime{}, false
	}
	r := Realtime{LifeCount: uint16(lc), Glucose: g, Temperature: 2590}
	if prev := st.reading(lc - 5); prev != 0 {
		r.Rate = (float64(g) - float64(prev)) / 5
	}
	if h := committedHistoric(lc); h >= st.WarmupMinutes {
		r.HistoricalLifeCount = uint16(h)
		r.HistoricalGlucose = st.reading(h)
	}
	return r, true
}

// historicPages are the pages answering a historical backfill request for
// life counts >= from.
func (s *Sensor) historicPages(from int, now time.Time) [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state
	last := committedHistoric(st.lifeCount(now))
	if last >= st.WearMinutes {
		last = st.WearMinutes - st.WearMinutes%5
	}
	start := from + (5-from%5)%5
	if start < st.WarmupMinutes {
		start = st.WarmupMinutes + (5-st.WarmupMinutes%5)%5
	}
	var pages [][]byte
	for p := start; p <= last; p += 30 {
		var values [6]uint16
		for i := range values {
			if lc := p + 5*i; lc <= last {
				values[i] = st.reading(lc)
			}
		}
		pages = append(pages, historicPage(uint16(p), values))
	}
	return pages
}

// clinicalRecords are the per-minute clinical records for life counts in
// [from, to], skipping warmup.
func (s *Sensor) clinicalRecords(from, to int) [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state
	if from < st.WarmupMinutes {
		from = st.WarmupMinutes
	}
	if to >= st.WearMinutes {
		to = st.WearMinutes - 1
	}
	var out [][]byte
	for lc := from; lc <= to; lc++ {
		h := uint16(0)
		if hl := committedHistoric(lc); hl >= st.WarmupMinutes {
			h = st.reading(hl)
		}
		out = append(out, clinicalRecord(uint16(lc), st.reading(lc), h))
	}
	return out
}

func (s *Sensor) clinicalSent() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int(s.state.ClinicalSent)
}

func (s *Sensor) setClinicalSent(lc int) {
	s.mu.Lock()
	if lc > int(s.state.ClinicalSent) {
		s.state.ClinicalSent = uint16(lc)
	}
	s.mu.Unlock()
}

// Status is the sensor as the control API reports it.
func (s *Sensor) Status() map[string]interface{} {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state
	lc := st.lifeCount(now)
	out := map[string]interface{}{
		"serial":         st.Serial,
		"bleAddress":     st.BLEAddress,
		"advertisedName": st.AdvertisedName(),
		"nfcState":       s.nfcState(now),
		"wearMinutes":    st.WearMinutes,
		"warmupMinutes":  st.WarmupMinutes,
		"receiverID":     fmt.Sprintf("%08x", st.ReceiverID),
		"pin":            st.PIN,
		"pairingOpen":    st.PairingOpen,
		"hasAuthKey":     st.AuthKey != "",
		"connected":      s.connected,
		"advertising":    s.reachable(now) && !s.connected,
		"glucoseMode":    st.Glucose,
	}
	if st.Activated != nil {
		out["activated"] = st.Activated.Format(time.RFC3339)
		out["lifeCount"] = lc
		if g := st.reading(lc); g != 0 {
			out["glucose"] = g
		}
	}
	if st.Terminated != nil {
		out["terminated"] = st.Terminated
	}
	if st.LastEvent != nil {
		out["lastEvent"] = st.LastEvent
	}
	if now.Before(s.outUntil) {
		out["outOfRangeUntil"] = s.outUntil.Format(time.RFC3339)
	}
	return out
}
