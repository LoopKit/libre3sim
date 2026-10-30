package main

import (
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/loopkit/libre3sim/libre3"
)

// serveAPI exposes the sensor's state and lets a tester steer it:
//
//	GET  /status                 the sensor and its latest reading
//	POST /glucose     {"kind":"fixed","value":65}
//	                  {"kind":"sine","value":120,"amplitude":40,"periodMinutes":240}
//	                  {"kind":"walk","value":140}
//	POST /disconnect             the sensor drops the link; the app reconnects
//	POST /out-of-range {"minutes":20}
//	                             drop the link and stay unreachable that long
//	POST /terminate   {"errorData":6}
//	                             end the sensor early (6 terminated, 8 replace sensor)
//	POST /event       {"errorData":8}
//	                             log an event-log entry while the sensor keeps working;
//	                             patch status repeats it with patch state 4 ({"errorData":0} clears)
//	POST /new         {"serial":"","wearDays":14,"warmupMinutes":60,"ageMinutes":0}
//	                             replace the sensor with a new one and restart;
//	                             ageMinutes 0 is a sensor still in its applicator
//	POST /force-quit             the app's end is a force-quit, not a termination
//
// and what an app in the Simulator needs in place of the sensor's NFC tag and
// Abbott's trust root:
//
//	POST /nfc         {"code":161,"parameters":"<hex>"} -> {"response":"<hex>"}
//	                             an ISO 15693 custom command (a1, a0, a8)
//	GET  /app-credentials        the credential file for AppIdentities.install(json:)
//	GET  /patch-signing-key      {"publicKey":"04..."}: the key that signs sensor certificates
func serveAPI(addr string, sensor *libre3.Sensor, radio *SimRadio, replace func(sensorConfig)) {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(indexHTML))
	})
	http.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, sensor.Status())
	})
	post("/glucose", func(w http.ResponseWriter, r *http.Request) {
		var m libre3.GlucoseMode
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil || m.Kind == "" {
			http.Error(w, "bad glucose mode", http.StatusBadRequest)
			return
		}
		sensor.SetGlucoseMode(m)
		log.Printf("glucose mode now %s", m)
		writeJSON(w, sensor.Status())
	})
	post("/disconnect", func(w http.ResponseWriter, r *http.Request) {
		if !radio.Disconnect() {
			http.Error(w, "no phone is connected", http.StatusConflict)
			return
		}
		log.Printf("dropped the link")
		writeJSON(w, sensor.Status())
	})
	post("/out-of-range", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Minutes float64 `json:"minutes"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Minutes <= 0 {
			http.Error(w, `want {"minutes":n}`, http.StatusBadRequest)
			return
		}
		sensor.GoOutOfRange(time.Duration(req.Minutes * float64(time.Minute)))
		radio.Disconnect()
		log.Printf("out of range for %.0f minutes", req.Minutes)
		writeJSON(w, sensor.Status())
	})
	post("/terminate", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ErrorData uint16 `json:"errorData"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ErrorData == 0 {
			req.ErrorData = 6
		}
		sensor.Terminate(req.ErrorData, time.Now())
		log.Printf("terminated with errorData %d", req.ErrorData)
		writeJSON(w, sensor.Status())
	})
	post("/event", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ErrorData uint16 `json:"errorData"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `want {"errorData":n}`, http.StatusBadRequest)
			return
		}
		sensor.LogEvent(req.ErrorData, time.Now())
		log.Printf("event-log entry with errorData %d", req.ErrorData)
		writeJSON(w, sensor.Status())
	})
	post("/new", func(w http.ResponseWriter, r *http.Request) {
		var c sensorConfig
		if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
			http.Error(w, "bad sensor config: "+err.Error(), http.StatusBadRequest)
			return
		}
		if _, err := c.state(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]string{"status": "restarting with a new sensor"})
		go func() {
			time.Sleep(300 * time.Millisecond)
			replace(c)
		}()
	})
	post("/nfc", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Code       byte   `json:"code"`
			Parameters string `json:"parameters"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad NFC command", http.StatusBadRequest)
			return
		}
		params, err := hex.DecodeString(req.Parameters)
		if err != nil {
			http.Error(w, "parameters: want hex", http.StatusBadRequest)
			return
		}
		resp, err := sensor.NFC(req.Code, params, time.Now())
		if err != nil {
			log.Printf("NFC %02x: %v", req.Code, err)
			code := 0
			if e, ok := err.(*libre3.NFCError); ok {
				code = int(e.Code)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			json.NewEncoder(w).Encode(map[string]interface{}{"error": err.Error(), "code": code})
			return
		}
		log.Printf("NFC %02x %x -> %x", req.Code, params, resp)
		writeJSON(w, map[string]string{"response": hex.EncodeToString(resp)})
	})
	http.HandleFunc("/app-credentials", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, sensor.Keys().AppCredentials())
	})
	http.HandleFunc("/patch-signing-key", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{"publicKey": hex.EncodeToString(sensor.Keys().SigningPublicKey())})
	})
	serveForceQuit(radio)
	log.Printf("API on %s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

func post(path string, f http.HandlerFunc) {
	http.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST to "+path, http.StatusMethodNotAllowed)
			return
		}
		f(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}
