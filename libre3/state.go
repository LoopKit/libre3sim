package libre3

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math"
	"math/rand"
	"os"
	"strings"
	"time"
)

// Patch states and error codes as RoundWhiteDiscKit's PatchStatus reads them.
const (
	patchActive     = 4
	patchExpired    = 5
	patchTerminated = 6

	errorNone       = 0
	errorExpired    = 5
	errorTerminated = 6
)

// NFC state bytes. Only 0x01 (not yet activated) is grounded; RoundWhiteDiscKit
// sends switch-receiver for anything else. The rest follow the Libre 2.
const (
	nfcNotActivated = 0x01
	nfcWarmup       = 0x02
	nfcActive       = 0x03
	nfcExpired      = 0x04
	nfcTerminated   = 0x05
)

// GlucoseMode is how readings are made up: "sine" around Value, "fixed" at
// Value, or "walk", a mean-reverting random walk around Value.
type GlucoseMode struct {
	Kind          string  `json:"kind"`
	Value         float64 `json:"value"`
	Amplitude     float64 `json:"amplitude,omitempty"`
	PeriodMinutes float64 `json:"periodMinutes,omitempty"`
}

func (m GlucoseMode) String() string { return fmt.Sprintf("%s %.0f", m.Kind, m.Value) }

// Termination is a sensor that has ended early: it reports ErrorData from
// LifeCount on, and stops advertising once its link closes.
type Termination struct {
	LifeCount  uint16 `json:"lifeCount"`
	ErrorData  uint16 `json:"errorData"`
	PatchState uint8  `json:"patchState"`
}

type State struct {
	Serial          string       `json:"serial"`
	BLEAddress      string       `json:"bleAddress"` // display order, e.g. E4:64:47:6E:2C:FE
	StaticKey       string       `json:"staticKey"`  // the sensor's P-256 scalar, hex
	WearMinutes     int          `json:"wearMinutes"`
	WarmupMinutes   int          `json:"warmupMinutes"`
	ProductType     uint8        `json:"productType"`
	SecurityVersion uint16       `json:"securityVersion"`
	Region          uint16       `json:"region"`
	Generation      uint16       `json:"generation"`
	Firmware        [4]byte      `json:"firmware"` // shown as b[3].b[2].b[1].b[0]
	Activated       *time.Time   `json:"activated,omitempty"`
	ReceiverID      uint32       `json:"receiverID"`
	PIN             string       `json:"pin"`               // hex; replaced by each activation or switch
	PairingOpen     bool         `json:"pairingOpen"`       // a full handshake is allowed: NFC ran since the last one
	AuthKey         string       `json:"authKey,omitempty"` // kAuth of the last full handshake, for cached reconnects
	Terminated      *Termination `json:"terminated,omitempty"`
	Glucose         GlucoseMode  `json:"glucose"`
	// Readings holds the glucose of every minute of wear, by life count; 0
	// where the sensor had none (warmup).
	Readings []uint16 `json:"readings"`
	// ClinicalSent is the last life count the clinical stream delivered; a
	// resubscribe replays from there.
	ClinicalSent uint16 `json:"clinicalSent"`
}

const serialAlphabet = "0123456789ABCDEFGHJKLMNPQRSTUVWXYZ"

// NewState makes a sensor fresh from its applicator: not yet activated. A
// non-zero age instead activates it that long ago, for a sensor that is
// already being worn. Its receiver is unclaimed (0): the first
// switch-receiver scan claims it, so any app can pick it up.
func NewState(serial string, wearDays int, warmup time.Duration, age time.Duration) (*State, error) {
	if serial == "" {
		var b strings.Builder
		b.WriteByte('3')
		for i := 0; i < 8; i++ {
			b.WriteByte(serialAlphabet[rand.Intn(len(serialAlphabet))])
		}
		serial = b.String()
	}
	if len(serial) != 9 {
		return nil, fmt.Errorf("serial %q: want 9 characters", serial)
	}
	addr := random(6)
	addr[0] |= 0xc0 // a random static address, as BLE wants
	parts := make([]string, 6)
	for i, b := range addr {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	s := &State{
		Serial:          serial,
		BLEAddress:      strings.Join(parts, ":"),
		StaticKey:       fmt.Sprintf("%x", randomScalar()),
		WearMinutes:     wearDays * 24 * 60,
		WarmupMinutes:   int(warmup / time.Minute),
		ProductType:     4,
		SecurityVersion: 1,
		Region:          1,
		Generation:      1,
		Firmware:        [4]byte{30, 2, 4, 1},
		PIN:             fmt.Sprintf("%x", random(4)),
		Glucose:         GlucoseMode{Kind: "sine", Value: 120, Amplitude: 40, PeriodMinutes: 240},
	}
	if age > 0 {
		at := time.Now().Add(-age).Truncate(time.Second)
		s.Activated = &at
	}
	return s, nil
}

// AdvertisedName is the BLE address without separators, which the sensor
// advertises as its local name.
func (s *State) AdvertisedName() string { return strings.ReplaceAll(s.BLEAddress, ":", "") }

func (s *State) lifeCount(now time.Time) int {
	if s.Activated == nil {
		return 0
	}
	return int(now.Sub(*s.Activated) / time.Minute)
}

// fill makes up readings through life count lc.
func (s *State) fill(lc int) {
	for len(s.Readings) <= lc && len(s.Readings) <= s.WearMinutes {
		i := len(s.Readings)
		if i < s.WarmupMinutes {
			s.Readings = append(s.Readings, 0)
			continue
		}
		v := s.model(i)
		s.Readings = append(s.Readings, uint16(math.Round(math.Max(40, math.Min(400, v)))))
	}
}

func (s *State) model(i int) float64 {
	m := s.Glucose
	switch m.Kind {
	case "fixed":
		return m.Value
	case "walk":
		prev := m.Value
		if n := len(s.Readings); n > 0 && s.Readings[n-1] != 0 {
			prev = float64(s.Readings[n-1])
		}
		return prev + (m.Value-prev)*0.02 + rand.NormFloat64()*0.8
	default:
		period := m.PeriodMinutes
		if period <= 0 {
			period = 240
		}
		return m.Value + m.Amplitude*math.Sin(2*math.Pi*float64(i)/period)
	}
}

func (s *State) reading(lc int) uint16 {
	if lc < 0 || lc >= len(s.Readings) {
		return 0
	}
	return s.Readings[lc]
}

func LoadState(path string) (*State, error) {
	b, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (s *State) Save(path string) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := ioutil.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
