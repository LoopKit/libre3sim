package libre3

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Captured frames and their plaintexts, from RoundWhiteDiscKit's DataPlaneTests.
func TestSealReproducesCapturedFrames(t *testing.T) {
	d := newDataPlane(unhex(t, "7c536d2ec3c76c741a776de85596959d"), unhex(t, "00000000bcc696a2"))
	cases := []struct {
		char      Char
		plaintext string
		frame     string
	}{
		{CharGlucose, "da386700e3ff0000ac26c73871000b670071001a0abf03f33c59100000",
			"3c937752c72e2dfdcfe05a5dce315b" + "6f3785f23dbc85066856d60027c5a41f45c90100"},
		{CharPatchStatus, "9d280000e0000704db381607", "4e6fe9573b6365476193e9c42009acd20100"},
		{CharHistoric, "98036d006b0069006e006a006500", "4637aa722274876d69a34f20096c49adced70100"},
	}
	for _, c := range cases {
		got := d.seal(c.char, unhex(t, c.plaintext))
		if want := unhex(t, c.frame); !bytes.Equal(got, want) {
			t.Errorf("%s: sealed %x, captured %x", c.char, got, want)
		}
		pt, err := d.open(c.char, got)
		if err != nil || !bytes.Equal(pt, unhex(t, c.plaintext)) {
			t.Errorf("%s: open: %x, %v", c.char, pt, err)
		}
	}
}

func TestRealtimeLayoutMatchesCapture(t *testing.T) {
	// The capture's lifeCount 14554 reading: 103 mg/dL falling 0.29/min,
	// historical 113 at 14535.
	r := Realtime{LifeCount: 14554, Glucose: 103, Rate: -0.29, HistoricalLifeCount: 14535, HistoricalGlucose: 113, Temperature: 0x0a1a}
	got := r.bytes()
	want := unhex(t, "da386700e3ff0000ac26c73871000b670071001a0abf03f33c59100000")
	// Bytes 21... are raw fast data the emulator leaves zero.
	if !bytes.Equal(got[:21], want[:21]) {
		t.Errorf("realtime\n got %x\nwant %x", got[:21], want[:21])
	}
	if committedHistoric(14554) != 14535 || committedHistoric(14563) != 14545 {
		t.Errorf("committed historic: %d %d", committedHistoric(14554), committedHistoric(14563))
	}
}

func TestPatchStatusLayout(t *testing.T) {
	p := PatchStatusRecord{LifeCount: 10397, EventData: 0xe0, Index: 7, PatchState: 4, CurrentLifeCount: 14555}
	got := p.bytes()
	if want := unhex(t, "9d280000e0000704db380000"); !bytes.Equal(got, want) {
		t.Errorf("patch status %x, want %x", got, want)
	}
}

func TestAbbottCRC16(t *testing.T) {
	if got := abbottCRC16(unhex(t, "fc2bee6978830d6f")); got != 0x71b4 {
		t.Errorf("crc %04x, want 71b4", got)
	}
}

func TestFragments(t *testing.T) {
	msg := make([]byte, 67)
	for i := range msg {
		msg[i] = byte(i)
	}
	frags := notifyFragments(msg)
	if len(frags) != 4 || frags[3][0] != 3 || len(frags[3]) != 1+67-57 {
		t.Fatalf("fragments %x", frags)
	}
	var w writeBuffer
	for off := 0; off < len(msg); off += writeChunk {
		end := off + writeChunk
		if end > len(msg) {
			end = len(msg)
		}
		if err := w.feed(append([]byte{byte(off), byte(off >> 8)}, msg[off:end]...)); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(w.data, msg) {
		t.Errorf("reassembled %x", w.data)
	}
}
