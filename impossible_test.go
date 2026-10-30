package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/loopkit/libre3sim/libre3"
)

// fakeApp is ImpossiBLE's library end of the socket.
type fakeApp struct {
	t    *testing.T
	conn net.Conn
	in   *bufio.Scanner
}

func (a *fakeApp) send(msg map[string]interface{}) {
	b, _ := json.Marshal(msg)
	a.conn.Write(append(b, '\n'))
}

func (a *fakeApp) await(kind string) map[string]interface{} {
	a.t.Helper()
	a.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for a.in.Scan() {
		var msg map[string]interface{}
		json.Unmarshal(a.in.Bytes(), &msg)
		if msg["type"] == kind {
			return msg
		}
	}
	a.t.Fatalf("no %s: %v", kind, a.in.Err())
	return nil
}

func TestScanConnectDiscover(t *testing.T) {
	dir := t.TempDir()
	keys, err := libre3.LoadOrCreateKeys(filepath.Join(dir, "keys.json"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := libre3.NewState("", 14, time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	sensor := libre3.NewSensor(st, keys, filepath.Join(dir, "state.json"))
	sock := filepath.Join(dir, "s.sock")
	r, err := NewSimRadio(sensor, sock)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Stop()
	go r.Run()

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	app := &fakeApp{t: t, conn: conn, in: bufio.NewScanner(conn)}
	app.send(map[string]interface{}{"type": "hello", "bundleId": "test.app", "pid": 0})
	app.send(map[string]interface{}{"type": "scan"})

	// An unactivated sensor is silent until an NFC scan starts it.
	params := make([]byte, 10)
	binary.LittleEndian.PutUint32(params[4:], 7)
	crc := crc16(params[:8])
	binary.LittleEndian.PutUint16(params[8:], crc)
	if _, err := sensor.NFC(0xa0, params, time.Now()); err != nil {
		t.Fatal(err)
	}
	seen := app.await("didDiscover")
	if seen["name"] != st.AdvertisedName() {
		t.Fatalf("advertised %v, want %s", seen["name"], st.AdvertisedName())
	}
	id := seen["id"].(string)
	app.send(map[string]interface{}{"type": "connect", "id": id})
	app.await("didConnect")
	app.send(map[string]interface{}{"type": "discoverServices", "id": id})
	services := app.await("didDiscoverServices")["services"].([]interface{})
	if len(services) != 3 {
		t.Fatalf("%d services", len(services))
	}
	data := services[0].(map[string]interface{})
	app.send(map[string]interface{}{"type": "discoverCharacteristics", "id": id, "serviceId": data["id"]})
	chars := app.await("didDiscoverCharacteristics")["characteristics"].([]interface{})
	if len(chars) != 7 {
		t.Fatalf("%d data characteristics", len(chars))
	}
	// Reading patch status before authorization answers empty.
	status := chars[1].(map[string]interface{})
	app.send(map[string]interface{}{"type": "read", "id": id, "characteristicId": status["id"]})
	if v := app.await("didUpdateValue"); v["characteristicId"] != status["id"] {
		t.Fatalf("read answered %v", v)
	}
	r.Disconnect()
	app.await("didDisconnect")
}

// crc16 is the NFC parameter checksum, as RoundWhiteDiscKit computes it.
func crc16(data []byte) uint16 {
	crc := uint16(0xffff)
	for _, b := range data {
		var rev byte
		for i := 0; i < 8; i++ {
			rev = rev<<1 | (b>>uint(i))&1
		}
		crc ^= uint16(rev) << 8
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
