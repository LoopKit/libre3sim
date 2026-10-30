package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/loopkit/libre3sim/libre3"
	"github.com/loopkit/libre3sim/suspend"
)

// SimRadio serves the sensor to an app in the iOS Simulator through
// ImpossiBLE (github.com/mickeyl/ImpossiBLE) instead of a Bluetooth adapter.
// The app links ImpossiBLE, whose CoreBluetooth shims speak newline-delimited
// JSON over a Unix socket; libre3sim takes the provider's end of that socket.
// Like ImpossiBLE's menu bar app it serves one client, and a new connection
// takes over from the old one. This is g7sim's transport with the Libre 3's
// GATT table and advertising: a Libre 3 advertises whenever it is reachable
// and not connected, and holds its one link.
type SimRadio struct {
	sensor   *libre3.Sensor
	path     string
	listener net.Listener
	id       string // the peripheral identifier the app sees
	stopped  bool

	mu          sync.Mutex
	client      *simClient
	scanning    bool
	pending     bool // the app asked to connect; held until the sensor advertises
	link        *simLink
	advertising bool
	lastSeen    time.Time // last advert reported to a scan
	lastLC      int

	unknown     map[string]bool // peripherals the app asked for that are not this sensor
	missedWakes int
	held        *heldRequests
	forceQuit   bool

	// Relaunch starts a terminated app again, as iOS does when a request it
	// is holding for the app completes. Nil only logs that it would.
	Relaunch func(bundleID string) error

	// Suspend, if set, stops the app while iOS would have it suspended.
	Suspend *suspend.Suspender
}

// heldRequests is what an app that iOS terminated left lodged. bluetoothd
// keeps them, relaunches the app when one completes and hands them back
// through state restoration. A force-quit drops them.
type heldRequests struct {
	bundleID string
	connect  bool
	scan     bool
	launched time.Time
}

type simClient struct {
	conn    net.Conn
	addr    string
	bundle  string
	pid     int
	restore *heldRequests
	wake    func()
	mu      sync.Mutex
	closed  bool
}

func (c *simClient) send(msg map[string]interface{}) {
	b, err := json.Marshal(msg)
	if err != nil {
		log.Printf("simulator: encoding %v: %v", msg["type"], err)
		return
	}
	if c.wake != nil {
		c.wake()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.conn.Write(append(b, '\n')); err != nil {
		log.Printf("simulator: send %v: %v", msg["type"], err)
	}
}

func (c *simClient) close() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.conn.Close()
}

type gattChar struct {
	uuid  string
	props int
	char  libre3.Char
}

type gattService struct {
	uuid  string
	chars []gattChar
}

const (
	propRead   = 0x02
	propWrite  = 0x08
	propNotify = 0x10
)

func libreUUID(prefix string) string { return prefix + "-EF89-11E9-81B4-2A2AE2DBCCE4" }

var dataServiceUUID = libreUUID("089810CC")

// The GATT layout RoundWhiteDiscKit's LibreSensorGATT names.
var simServices = []gattService{
	{dataServiceUUID, []gattChar{
		{libreUUID("0898177A"), propNotify, libre3.CharGlucose},
		{libreUUID("08981482"), propRead | propNotify, libre3.CharPatchStatus},
		{libreUUID("08981338"), propWrite | propNotify, libre3.CharPatchControl},
		{libreUUID("0898195A"), propNotify, libre3.CharHistoric},
		{libreUUID("08981BEE"), propNotify, libre3.CharEventLog},
		{libreUUID("08981AB8"), propNotify, libre3.CharClinical},
		{libreUUID("08981D24"), propNotify, libre3.CharFactory},
	}},
	{libreUUID("0898203A"), []gattChar{
		{libreUUID("089823FA"), propWrite | propNotify, libre3.CharCert},
		{libreUUID("089822CE"), propWrite | propNotify, libre3.CharChallenge},
		{libreUUID("08982198"), propWrite | propNotify, libre3.CharCommand},
	}},
	{libreUUID("08982400"), nil},
}

// peripheralID derives the identifier from the BLE address, so the app finds
// the same peripheral again after either side restarts.
func peripheralID(name string) string {
	h := sha256.Sum256([]byte("libre3sim " + name))
	h[6] = h[6]&0x0f | 0x40
	h[8] = h[8]&0x3f | 0x80
	x := strings.ToUpper(hex.EncodeToString(h[:16]))
	return x[0:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:32]
}

// centralAddr stands in for the phone's Bluetooth address. One per app.
func centralAddr(bundleID string) string {
	h := sha256.Sum256([]byte(bundleID))
	return fmt.Sprintf("%02X:%02X:%02X:%02X:%02X:%02X", h[0]|0xC0, h[1], h[2], h[3], h[4], h[5])
}

func NewSimRadio(sensor *libre3.Sensor, path string) (*SimRadio, error) {
	if c, err := net.Dial("unix", path); err == nil {
		c.Close()
		return nil, fmt.Errorf("%s is served by another provider; quit it (ImpossiBLE's menu bar app?) first", path)
	}
	os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	r := &SimRadio{sensor: sensor, path: path, listener: l, id: peripheralID(sensor.AdvertisedName()), lastLC: -1,
		unknown: map[string]bool{}}
	log.Printf("serving the iOS Simulator on %s as peripheral %s", path, r.id)
	go r.accept()
	return r, nil
}

func (r *SimRadio) accept() {
	for {
		conn, err := r.listener.Accept()
		if err != nil {
			r.mu.Lock()
			stopped := r.stopped
			r.mu.Unlock()
			if !stopped {
				log.Printf("simulator: accept: %v", err)
			}
			return
		}
		go r.serve(&simClient{conn: conn})
	}
}

func (r *SimRadio) serve(c *simClient) {
	sc := bufio.NewScanner(c.conn)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var msg map[string]interface{}
		if err := json.Unmarshal(sc.Bytes(), &msg); err != nil {
			log.Printf("simulator: bad message: %v", err)
			continue
		}
		r.handle(c, msg)
	}
	r.dropClient(c, true)
}

func (r *SimRadio) dropClient(c *simClient, ended bool) {
	r.mu.Lock()
	var held *heldRequests
	if r.client == c {
		if ended && !r.forceQuit && c.bundle != "" && (r.pending || r.scanning) {
			held = &heldRequests{bundleID: c.bundle, connect: r.pending, scan: r.scanning}
			r.held = held
		}
		r.forceQuit = false
		r.client = nil
		r.scanning = false
		r.pending = false
	}
	l := r.link
	if l != nil && l.client == c {
		r.link = nil
	} else {
		l = nil
	}
	r.mu.Unlock()
	if held != nil {
		log.Printf("simulator: %s ended with %s lodged; holding it as iOS would for a terminated app", c.bundle, held)
	}
	if r.Suspend != nil && c.pid > 0 {
		r.Suspend.Detach(c.pid)
	}
	if l != nil {
		l.end(false)
	}
	c.close()
}

func (h *heldRequests) String() string {
	switch {
	case h.connect && h.scan:
		return "a connect and a scan"
	case h.connect:
		return "a connect"
	default:
		return "a scan"
	}
}

func str(msg map[string]interface{}, key string) string {
	s, _ := msg[key].(string)
	return s
}

func (r *SimRadio) current(c *simClient) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.client == c
}

func (r *SimRadio) takeOver(c *simClient) {
	r.mu.Lock()
	old := r.client
	r.client = c
	r.mu.Unlock()
	if old != nil {
		log.Printf("simulator: a new app connected; dropping the previous one")
		old.send(map[string]interface{}{"type": "connectionRejected", "code": "clientBusy",
			"message": "another app connected to libre3sim"})
		r.dropClient(old, false)
	}
}

func (r *SimRadio) restoreState(c *simClient) {
	h := c.restore
	c.restore = nil
	ids, services := []string{}, []string{}
	if h.connect {
		ids = append(ids, r.id)
	}
	if h.scan {
		services = append(services, dataServiceUUID)
	}
	c.send(map[string]interface{}{"type": "didRestoreState", "peripheralIds": ids,
		"scanServices": services, "scanOptions": map[string]interface{}{}})
	r.mu.Lock()
	r.pending = r.pending || h.connect
	r.scanning = r.scanning || h.scan
	r.mu.Unlock()
	log.Printf("simulator: restored %s for %s", h, c.bundle)
}

// ForceQuit drops what a terminated app left lodged, as iOS does when the
// user swipes the app away. Called while the app runs, it applies to the
// app's end.
func (r *SimRadio) ForceQuit() {
	r.mu.Lock()
	h := r.held
	r.held = nil
	r.forceQuit = r.client != nil
	r.mu.Unlock()
	if h != nil {
		log.Printf("simulator: %s force-quit; dropped its held %s", h.bundleID, h)
	}
}

// Disconnect is the sensor dropping the link, as it does when the phone
// walks out of range for a moment.
func (r *SimRadio) Disconnect() bool {
	r.mu.Lock()
	l := r.link
	r.mu.Unlock()
	if l == nil {
		return false
	}
	l.Close()
	return true
}

func (r *SimRadio) handle(c *simClient, msg map[string]interface{}) {
	t := str(msg, "type")
	if t == "hello" {
		c.bundle = str(msg, "bundleId")
		c.addr = centralAddr(c.bundle)
		pid, _ := msg["pid"].(float64)
		c.pid = int(pid)
		log.Printf("simulator: %s (ImpossiBLE %s, pid %v) connected as %s",
			c.bundle, str(msg, "clientVersion"), msg["pid"], c.addr)
		if s := r.Suspend; s != nil && c.pid > 0 {
			s.Attach(c.pid, c.bundle)
			c.wake = func() { s.Wake(c.pid) }
		}
		r.takeOver(c)
		r.mu.Lock()
		if r.held != nil && r.held.bundleID == c.bundle {
			c.restore = r.held
			r.held = nil
		}
		r.mu.Unlock()
		return
	}
	if !r.current(c) {
		return
	}
	if c.restore != nil {
		r.restoreState(c)
	}
	id := str(msg, "id")
	switch t {
	case "scan":
		r.mu.Lock()
		r.scanning = true
		r.lastSeen = time.Time{}
		r.mu.Unlock()
	case "stopScan":
		r.mu.Lock()
		r.scanning = false
		r.mu.Unlock()
	case "connect":
		if id != r.id {
			// A peripheral that is not here (an earlier sensor): iOS holds
			// the connect until it comes into range, which it never will.
			r.mu.Lock()
			first := !r.unknown[id]
			r.unknown[id] = true
			r.mu.Unlock()
			if first {
				log.Printf("simulator: holding a connect to %s, which is not this sensor", id)
			}
			return
		}
		r.mu.Lock()
		if r.link == nil {
			r.pending = true
		}
		r.mu.Unlock()
		r.tryConnect(time.Now())
	case "cancel":
		r.mu.Lock()
		r.pending = false
		l := r.link
		r.link = nil
		r.mu.Unlock()
		if l != nil {
			l.end(true)
		} else {
			c.send(disconnected(id))
		}
	case "discoverServices":
		var services []map[string]interface{}
		for i, s := range simServices {
			services = append(services, map[string]interface{}{
				"id": fmt.Sprintf("%s:%s:%d", r.id, s.uuid, i), "uuid": s.uuid, "primary": true})
		}
		c.send(map[string]interface{}{"type": "didDiscoverServices", "id": id, "services": services, "error": ""})
	case "discoverCharacteristics":
		serviceID := str(msg, "serviceId")
		chars := []map[string]interface{}{}
		for si, s := range simServices {
			if serviceID != fmt.Sprintf("%s:%s:%d", r.id, s.uuid, si) {
				continue
			}
			for ci, ch := range s.chars {
				chars = append(chars, map[string]interface{}{
					"id": fmt.Sprintf("%s:%s:%d", serviceID, ch.uuid, ci), "uuid": ch.uuid, "properties": ch.props})
			}
		}
		c.send(map[string]interface{}{"type": "didDiscoverCharacteristics", "id": id,
			"serviceId": serviceID, "characteristics": chars, "error": ""})
	case "discoverDescriptors":
		c.send(map[string]interface{}{"type": "didDiscoverDescriptors", "id": id,
			"characteristicId": str(msg, "characteristicId"), "descriptors": []interface{}{}, "error": ""})
	case "discoverIncludedServices":
		c.send(map[string]interface{}{"type": "didDiscoverIncludedServices", "id": id,
			"serviceId": str(msg, "serviceId"), "includedServices": []interface{}{}, "error": ""})
	case "read":
		r.read(c, msg)
	case "write":
		r.write(c, msg)
	case "setNotify":
		r.setNotify(c, msg)
	case "readRSSI":
		c.send(map[string]interface{}{"type": "didReadRSSI", "id": id, "rssi": -60, "error": ""})
	case "registerForConnectionEvents":
	case "setMockConfiguration":
		c.send(map[string]interface{}{"type": "didSetMockConfiguration", "ok": false,
			"error": "libre3sim serves its own sensor and takes no mock configuration"})
	case "clearMockConfiguration":
	case "openL2CAP":
		c.send(map[string]interface{}{"type": "didOpenL2CAP", "id": id, "channelId": "", "psm": msg["psm"],
			"error": "libre3sim has no L2CAP channels"})
	default:
		log.Printf("simulator: ignoring %s", t)
	}
}

func disconnected(id string) map[string]interface{} {
	return map[string]interface{}{"type": "didDisconnect", "id": id, "error": "",
		"timestamp": float64(time.Now().UnixNano())/1e9 - 978307200, "isReconnecting": false}
}

// discovered reports the advertisement as a scan sees it: the BLE address
// as the local name, and the data service.
func (r *SimRadio) discovered(c *simClient) {
	name := r.sensor.AdvertisedName()
	c.send(map[string]interface{}{"type": "didDiscover", "id": r.id, "name": name, "rssi": -60,
		"adv": map[string]interface{}{
			"kCBAdvDataLocalName":     name,
			"kCBAdvDataIsConnectable": true,
			"kCBAdvDataServiceUUIDs":  []string{dataServiceUUID},
		}})
}

func (r *SimRadio) charFor(charID string) (gattChar, bool) {
	for si, s := range simServices {
		prefix := fmt.Sprintf("%s:%s:%d:", r.id, s.uuid, si)
		if !strings.HasPrefix(charID, prefix) {
			continue
		}
		for ci, ch := range s.chars {
			if charID == fmt.Sprintf("%s%s:%d", prefix, ch.uuid, ci) {
				return ch, true
			}
		}
	}
	return gattChar{}, false
}

func (r *SimRadio) activeLink(c *simClient) *simLink {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.link != nil && r.link.client == c {
		return r.link
	}
	return nil
}

func (r *SimRadio) read(c *simClient, msg map[string]interface{}) {
	charID := str(msg, "characteristicId")
	ch, ok := r.charFor(charID)
	l := r.activeLink(c)
	if !ok || l == nil {
		c.send(map[string]interface{}{"type": "didUpdateValue", "id": str(msg, "id"),
			"characteristicId": charID, "value": "", "error": "not connected"})
		return
	}
	log.Printf("%s read %s", l.addr, ch.char)
	l.handler.Read(ch.char)
}

func (r *SimRadio) write(c *simClient, msg map[string]interface{}) {
	id, charID := str(msg, "id"), str(msg, "characteristicId")
	data, err := base64.StdEncoding.DecodeString(str(msg, "value"))
	ch, ok := r.charFor(charID)
	l := r.activeLink(c)
	reply := map[string]interface{}{"type": "didWriteValue", "id": id, "characteristicId": charID, "error": ""}
	switch {
	case err != nil:
		reply["error"] = "bad value"
	case !ok:
		reply["error"] = "unknown characteristic"
	case l == nil:
		reply["error"] = "not connected"
	}
	if wt, _ := msg["writeType"].(float64); wt == 0 {
		c.send(reply)
	}
	if reply["error"] != "" {
		return
	}
	l.handler.Write(ch.char, data)
}

func (r *SimRadio) setNotify(c *simClient, msg map[string]interface{}) {
	id, charID := str(msg, "id"), str(msg, "characteristicId")
	enabled, _ := msg["enabled"].(bool)
	if n, ok := msg["enabled"].(float64); ok {
		enabled = n != 0
	}
	ch, ok := r.charFor(charID)
	l := r.activeLink(c)
	reply := map[string]interface{}{"type": "didUpdateNotification", "id": id, "characteristicId": charID,
		"enabled": enabled, "error": ""}
	if !ok {
		reply["error"] = "unknown characteristic"
	} else if l == nil {
		reply["error"] = "not connected"
	}
	if reply["error"] != "" {
		reply["enabled"] = false
		c.send(reply)
		return
	}
	l.mu.Lock()
	if enabled {
		l.notifying[ch.char] = charID
	} else {
		delete(l.notifying, ch.char)
	}
	l.mu.Unlock()
	c.send(reply)
	log.Printf("%s %s notifications %v", l.addr, ch.char, map[bool]string{true: "on", false: "off"}[enabled])
	l.handler.Subscribed(ch.char, enabled)
}

// tryConnect brings up a pending connection while the sensor advertises, as
// iOS completes a pending connect on the next advert it hears.
func (r *SimRadio) tryConnect(now time.Time) {
	r.mu.Lock()
	c := r.client
	if !r.pending || c == nil || r.link != nil || !r.sensor.Advertising(now) {
		r.mu.Unlock()
		return
	}
	r.pending = false
	l := &simLink{radio: r, client: c, addr: c.addr, notifying: map[libre3.Char]string{}}
	r.link = l
	r.mu.Unlock()

	r.sensor.LinkOpened()
	log.Printf("%s connected", l.addr)
	c.send(map[string]interface{}{"type": "didConnect", "id": r.id})
	l.handler = libre3.NewHandler(r.sensor, l)
}

func (r *SimRadio) Run() {
	for now := range time.Tick(100 * time.Millisecond) {
		lc := r.sensor.Tick(now)
		advertising := r.sensor.Advertising(now)
		r.mu.Lock()
		changed := advertising != r.advertising
		r.advertising = advertising
		c, scanning := r.client, r.scanning
		report := advertising && scanning && c != nil && now.Sub(r.lastSeen) > 5*time.Second
		if report {
			r.lastSeen = now
		}
		missed := 0
		var relaunch *heldRequests
		if advertising && c != nil && lc != r.lastLC {
			if !r.pending && !scanning && r.link == nil && r.held == nil {
				r.missedWakes++
				missed = r.missedWakes
			}
			if c == nil && r.held != nil && now.Sub(r.held.launched) > 20*time.Second {
				r.held.launched = now
				relaunch = r.held
			}
		}
		r.lastLC = lc
		r.mu.Unlock()
		if changed {
			log.Printf("advertising %v", advertising)
		}
		if missed > 0 {
			log.Printf("NO WAKE PATH: the sensor is advertising and the app has no pending connect or scan; a backgrounded app would never hear from it (%d minutes so far)", missed)
		}
		if relaunch != nil {
			r.relaunch(relaunch)
		}
		if report {
			r.discovered(c)
		}
		r.tryConnect(now)
	}
}

func (r *SimRadio) relaunch(h *heldRequests) {
	if r.Relaunch == nil {
		log.Printf("simulator: iOS would relaunch %s now for its held %s (start libre3sim with -simulator and -relaunch to do it)", h.bundleID, h)
		return
	}
	log.Printf("simulator: relaunching %s for its held %s", h.bundleID, h)
	go func() {
		if err := r.Relaunch(h.bundleID); err != nil {
			log.Printf("simulator: relaunching %s: %v", h.bundleID, err)
		}
	}()
}

// Stop closes the socket, ending any link.
func (r *SimRadio) Stop() {
	r.mu.Lock()
	r.stopped = true
	r.mu.Unlock()
	r.listener.Close()
	r.mu.Lock()
	c := r.client
	r.mu.Unlock()
	if c != nil {
		r.dropClient(c, false)
	}
	os.Remove(r.path)
}

// simLink is the app's connection, as the handler sees it.
type simLink struct {
	radio   *SimRadio
	client  *simClient
	addr    string
	handler libre3.Handler

	mu        sync.Mutex
	notifying map[libre3.Char]string // characteristic id by char
	ended     bool
}

func (l *simLink) Addr() string { return l.addr }

func (l *simLink) charID(c libre3.Char) string {
	for si, s := range simServices {
		for ci, ch := range s.chars {
			if ch.char == c {
				return fmt.Sprintf("%s:%s:%d:%s:%d", l.radio.id, s.uuid, si, ch.uuid, ci)
			}
		}
	}
	return ""
}

func (l *simLink) Notify(c libre3.Char, b []byte) error {
	l.mu.Lock()
	charID, ok := l.notifying[c]
	ended := l.ended
	l.mu.Unlock()
	if ended {
		return errors.New("link closed")
	}
	if !ok {
		return errors.New("not subscribed")
	}
	log.Printf("%s %s -> %s", l.addr, c, hex.EncodeToString(b))
	l.client.send(map[string]interface{}{"type": "didUpdateValue", "id": l.radio.id,
		"characteristicId": charID, "value": base64.StdEncoding.EncodeToString(b), "error": ""})
	return nil
}

func (l *simLink) Respond(c libre3.Char, b []byte) {
	l.client.send(map[string]interface{}{"type": "didUpdateValue", "id": l.radio.id,
		"characteristicId": l.charID(c), "value": base64.StdEncoding.EncodeToString(b), "error": ""})
}

// Close is the sensor hanging up.
func (l *simLink) Close() {
	r := l.radio
	r.mu.Lock()
	if r.link == l {
		r.link = nil
	}
	r.mu.Unlock()
	l.end(true)
}

// end tears the link down once, telling the app if it is still there.
func (l *simLink) end(tell bool) {
	l.mu.Lock()
	if l.ended {
		l.mu.Unlock()
		return
	}
	l.ended = true
	l.mu.Unlock()
	log.Printf("%s disconnected", l.addr)
	if l.handler != nil {
		l.handler.Closed()
	}
	l.radio.sensor.LinkClosed()
	if tell {
		l.client.send(disconnected(l.radio.id))
	}
}

// simctlRelauncher launches the app on a simulator with ImpossiBLE injected.
func simctlRelauncher(simulator, dylib string) func(string) error {
	if dylib == "" {
		exe, err := os.Executable()
		if err == nil {
			dylib = filepath.Join(filepath.Dir(exe), "simulator", "build", "libImpossiBLE.dylib")
		}
	}
	return func(bundleID string) error {
		cmd := exec.Command("xcrun", "simctl", "launch", simulator, bundleID)
		cmd.Env = append(os.Environ(), "SIMCTL_CHILD_DYLD_INSERT_LIBRARIES="+dylib)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
}

func serveForceQuit(r *SimRadio) {
	http.HandleFunc("/force-quit", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			http.Error(w, "POST to force-quit", http.StatusMethodNotAllowed)
			return
		}
		r.ForceQuit()
		w.WriteHeader(http.StatusNoContent)
	})
}
