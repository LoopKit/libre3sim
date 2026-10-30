package main

import (
	"flag"
	"log"
	"math/rand"
	"os"
	"strconv"
	"syscall"
	"time"

	"github.com/loopkit/libre3sim/libre3"
	"github.com/loopkit/libre3sim/suspend"
)

// sensorConfig describes a new sensor. Zero values pick defaults.
type sensorConfig struct {
	Serial        string   `json:"serial"`
	WearDays      int      `json:"wearDays"`
	WarmupMinutes *float64 `json:"warmupMinutes"`
	AgeMinutes    float64  `json:"ageMinutes"`
}

func (c sensorConfig) state() (*libre3.State, error) {
	if c.WearDays == 0 {
		c.WearDays = 14
	}
	warmup := time.Hour
	if c.WarmupMinutes != nil {
		warmup = time.Duration(*c.WarmupMinutes * float64(time.Minute))
	}
	return libre3.NewState(c.Serial, c.WearDays, warmup, time.Duration(c.AgeMinutes*float64(time.Minute)))
}

func main() {
	statePath := flag.String("state", "libre3state.json", "sensor state file")
	keysPath := flag.String("keys", "libre3sim-keys.json", "the emulator's signing key and app credential, kept across sensors")
	fresh := flag.Bool("fresh", false, "start a new sensor instead of restoring the state file")
	serial := flag.String("serial", "", "9-character serial for a new sensor (default: random)")
	age := flag.Duration("age", 0, "how long ago a new sensor was activated; 0 leaves it in its applicator, for NFC activation")
	days := flag.Int("days", 14, "wear duration of a new sensor, in days")
	warmup := flag.Duration("warmup", time.Hour, "warmup of a new sensor")
	api := flag.String("api", ":8080", "HTTP control API address")
	socket := flag.String("socket", "/tmp/impossible.sock", "ImpossiBLE socket")
	simulator := flag.String("simulator", "", "the app's simulator (name or UDID), for -relaunch and -suspend")
	relaunch := flag.Bool("relaunch", false, "relaunch a terminated app for its lodged connect, as iOS does")
	suspendApp := flag.Bool("suspend", false, "stop the app while iOS would have it suspended, waking it for Bluetooth events")
	dylib := flag.String("dylib", "", "ImpossiBLE library injected into a relaunched app (default: simulator/build/libImpossiBLE.dylib beside libre3sim)")
	flag.Parse()
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	rand.Seed(time.Now().UnixNano())

	keys, err := libre3.LoadOrCreateKeys(*keysPath)
	if err != nil {
		log.Fatalf("keys %s: %v", *keysPath, err)
	}
	var state *libre3.State
	if !*fresh {
		s, err := libre3.LoadState(*statePath)
		if err != nil && !os.IsNotExist(err) {
			log.Fatalf("restoring %s: %v (use -fresh to start a new sensor)", *statePath, err)
		}
		state = s
	}
	if state == nil {
		c := sensorConfig{Serial: *serial, WearDays: *days, AgeMinutes: age.Minutes()}
		m := warmup.Minutes()
		c.WarmupMinutes = &m
		s, err := c.state()
		if err != nil {
			log.Fatal(err)
		}
		if err := s.Save(*statePath); err != nil {
			log.Fatalf("saving %s: %v", *statePath, err)
		}
		state = s
		log.Printf("new sensor")
	}

	log.Printf("libre3sim — for development use only, not an Abbott product; simulated data only")
	activated := "not yet activated (scan it to start it)"
	if state.Activated != nil {
		activated = "activated " + state.Activated.Format(time.RFC3339)
	}
	log.Printf("serial %s, BLE address %s, %d-day wear, %d-minute warmup, %s",
		state.Serial, state.BLEAddress, state.WearMinutes/1440, state.WarmupMinutes, activated)

	sensor := libre3.NewSensor(state, keys, *statePath)
	radio, err := NewSimRadio(sensor, *socket)
	if err != nil {
		log.Fatalf("serving %s: %v", *socket, err)
	}
	if (*relaunch || *suspendApp) && *simulator == "" {
		log.Fatalf("-relaunch and -suspend need -simulator")
	}
	if *relaunch {
		radio.Relaunch = simctlRelauncher(*simulator, *dylib)
	}
	if *suspendApp {
		radio.Suspend = suspend.New(*simulator)
	}

	// replace swaps in a new sensor: it is saved over the state file and the
	// emulator re-executes itself, so the new sensor starts exactly as a
	// normal launch does, with its own address and peripheral.
	replace := func(c sensorConfig) {
		next, err := c.state()
		if err != nil {
			log.Printf("new sensor: %v", err)
			return
		}
		sensor.Retire()
		radio.Stop()
		if err := next.Save(*statePath); err != nil {
			log.Fatalf("saving %s: %v", *statePath, err)
		}
		exe, err := os.Executable()
		if err != nil {
			log.Fatalf("restarting: %v", err)
		}
		log.Printf("restarting as the new sensor")
		args := []string{exe, "-state", *statePath, "-keys", *keysPath, "-api", *api, "-socket", *socket,
			"-simulator", *simulator, "-relaunch=" + strconv.FormatBool(*relaunch),
			"-suspend=" + strconv.FormatBool(*suspendApp), "-dylib", *dylib}
		log.Fatalf("restarting: %v", syscall.Exec(exe, args, os.Environ()))
	}

	go serveAPI(*api, sensor, radio, replace)
	radio.Run()
}
