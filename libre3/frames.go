package libre3

import (
	"encoding/binary"
	"errors"
	"math"
)

// The wire layouts here are what RoundWhiteDiscKit decodes: BleFraming,
// DataPlaneCrypto, PatchStatus, RealtimeGlucoseReading, HistoricalReadingPage
// and ClinicalReadingRecord. The emulator is right when that code reads them.

const (
	writeChunk  = 18 // phone writes: offset_LE2 || up to 18 bytes
	notifyChunk = 19 // sensor notifies: seq1 || up to 19 bytes
)

// notifyFragments splits a handshake message into sequence-numbered notifies.
func notifyFragments(msg []byte) [][]byte {
	var out [][]byte
	for i, seq := 0, byte(0); i < len(msg); i, seq = i+notifyChunk, seq+1 {
		end := i + notifyChunk
		if end > len(msg) {
			end = len(msg)
		}
		out = append(out, append([]byte{seq}, msg[i:end]...))
	}
	return out
}

// writeBuffer reassembles a phone write from its offset-prefixed fragments.
type writeBuffer struct{ data []byte }

var errFragment = errors.New("write fragment out of order")

func (w *writeBuffer) feed(fragment []byte) error {
	if len(fragment) < 2 {
		return errFragment
	}
	offset := int(binary.LittleEndian.Uint16(fragment))
	if offset == 0 {
		w.data = w.data[:0]
	}
	if offset != len(w.data) {
		return errFragment
	}
	w.data = append(w.data, fragment[2:]...)
	return nil
}

// Packet descriptors in the data-plane nonce, by channel. patchStatus,
// glucoseData and historicData are the kinds RoundWhiteDiscKit prefers for
// them; the rest are guesses it accepts by trying every descriptor.
var descriptors = map[Char][]byte{
	CharPatchControl: {0x00, 0x00, 0x00},
	CharPatchStatus:  {0x00, 0x00, 0xf0},
	CharGlucose:      {0x00, 0x0f, 0x00},
	CharHistoric:     {0x00, 0xf0, 0x00},
	CharClinical:     {0x0f, 0x00, 0x00},
	CharEventLog:     {0xf0, 0x00, 0x00},
	CharFactory:      {0x44, 0x00, 0x00},
}

// dataPlane is one session's post-authorization encryption.
type dataPlane struct {
	kEnc, ivEnc []byte
	seq         map[Char]uint16
}

func newDataPlane(kEnc, ivEnc []byte) *dataPlane {
	return &dataPlane{kEnc: kEnc, ivEnc: ivEnc, seq: map[Char]uint16{}}
}

func dataNonce(seq uint16, descriptor, ivEnc []byte) []byte {
	n := []byte{byte(seq), byte(seq >> 8)}
	n = append(n, descriptor...)
	return append(n, ivEnc...)
}

// seal encrypts a payload for a channel: ciphertext || tag || seq_LE2.
func (d *dataPlane) seal(c Char, plaintext []byte) []byte {
	d.seq[c]++
	seq := d.seq[c]
	out := ccmSeal(d.kEnc, dataNonce(seq, descriptors[c], d.ivEnc), plaintext)
	return append(out, byte(seq), byte(seq>>8))
}

// open decrypts a frame the phone wrote on a channel.
func (d *dataPlane) open(c Char, frame []byte) ([]byte, error) {
	if len(frame) < tagLen+2 {
		return nil, errAuth
	}
	seq := binary.LittleEndian.Uint16(frame[len(frame)-2:])
	return ccmOpen(d.kEnc, dataNonce(seq, descriptors[c], d.ivEnc), frame[:len(frame)-2])
}

// PatchStatusRecord is the 12-byte patchStatus plaintext.
type PatchStatusRecord struct {
	LifeCount        uint16 // the last event's life count
	ErrorData        uint16
	EventData        uint16
	Index            uint8
	PatchState       uint8
	CurrentLifeCount uint16
}

func (p PatchStatusRecord) bytes() []byte {
	b := make([]byte, 12)
	binary.LittleEndian.PutUint16(b[0:], p.LifeCount)
	binary.LittleEndian.PutUint16(b[2:], p.ErrorData)
	binary.LittleEndian.PutUint16(b[4:], p.EventData)
	b[6] = p.Index
	b[7] = p.PatchState
	binary.LittleEndian.PutUint16(b[8:], p.CurrentLifeCount)
	return b
}

// Realtime is the 29-byte glucoseData plaintext.
type Realtime struct {
	LifeCount           uint16
	Glucose             uint16
	Rate                float64 // mg/dL/min
	HistoricalLifeCount uint16
	HistoricalGlucose   uint16 // 0 when no historical point is committed yet
	Temperature         uint16
}

// Trend values as RoundWhiteDiscKit's Libre3Trend reads them.
func trend(rate float64) byte {
	switch {
	case rate <= -2:
		return 1
	case rate <= -1:
		return 2
	case rate < 1:
		return 3
	case rate < 2:
		return 4
	default:
		return 5
	}
}

func (r Realtime) bytes() []byte {
	b := make([]byte, 29)
	binary.LittleEndian.PutUint16(b[0:], r.LifeCount)
	binary.LittleEndian.PutUint16(b[2:], r.Glucose)
	binary.LittleEndian.PutUint16(b[4:], uint16(int16(math.Round(r.Rate*100))))
	// Projected glucose: captures carry the 15-minute projection times 100.
	projected := math.Round(float64(r.Glucose)+15*r.Rate) * 100
	binary.LittleEndian.PutUint16(b[8:], uint16(math.Max(0, projected)))
	binary.LittleEndian.PutUint16(b[10:], r.HistoricalLifeCount)
	binary.LittleEndian.PutUint16(b[12:], r.HistoricalGlucose)
	b[14] = trend(r.Rate) | 0x08 // actionable
	binary.LittleEndian.PutUint16(b[15:], r.Glucose)
	binary.LittleEndian.PutUint16(b[17:], r.HistoricalGlucose)
	binary.LittleEndian.PutUint16(b[19:], r.Temperature)
	return b
}

// historicPage is the 14-byte historicData plaintext: a start life count and
// six samples five minutes apart, zero where none is committed.
func historicPage(start uint16, values [6]uint16) []byte {
	b := make([]byte, 14)
	binary.LittleEndian.PutUint16(b, start)
	for i, v := range values {
		binary.LittleEndian.PutUint16(b[2+2*i:], v)
	}
	return b
}

// clinicalRecord is the 14-byte clinicalData plaintext for one minute.
func clinicalRecord(lifeCount, current, historic uint16) []byte {
	b := make([]byte, 14)
	binary.LittleEndian.PutUint16(b[0:], lifeCount)
	binary.LittleEndian.PutUint16(b[2:], current*16) // raw channels: made up, rising with glucose
	binary.LittleEndian.PutUint16(b[4:], current*12)
	binary.LittleEndian.PutUint16(b[6:], 0x0e00)
	binary.LittleEndian.PutUint16(b[10:], current)
	binary.LittleEndian.PutUint16(b[12:], historic)
	return b
}
