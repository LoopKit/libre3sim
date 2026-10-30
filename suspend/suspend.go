package suspend

import (
	"bufio"
	"encoding/json"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// bluetoothWake is how long iOS lets a suspended app with the
// bluetooth-central background mode run for one Bluetooth event.
const bluetoothWake = 10 * time.Second

// Suspender makes a backgrounded app in the Simulator actually stop.
// runningboard decides when an app should be suspended, as on a phone, but
// cannot stop a Mac process; the suspender follows its verdicts with
// SIGSTOP and SIGCONT, and resumes the app for each Bluetooth event, as
// bluetoothd wakes it.
type Suspender struct {
	simulator string

	mu        sync.Mutex
	pid       int
	bundle    string
	suspended bool // runningboard's verdict
	wakeUntil time.Time
	stopped   bool
}

func New(simulator string) *Suspender {
	s := &Suspender{simulator: simulator}
	go s.watch()
	go func() {
		for range time.Tick(250 * time.Millisecond) {
			s.mu.Lock()
			s.apply(time.Now())
			s.mu.Unlock()
		}
	}()
	go func() {
		// Leave the app running on exit.
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		<-ch
		s.Release()
		os.Exit(1)
	}()
	return s
}

// attach follows a newly connected app.
func (s *Suspender) Attach(pid int, bundle string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resume()
	s.pid, s.bundle, s.suspended, s.wakeUntil = pid, bundle, false, time.Time{}
}

// detach stops following an app that ended or lost the socket.
func (s *Suspender) Detach(pid int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pid == pid {
		s.resume()
		s.pid = 0
	}
}

// wake gives the app a Bluetooth event's run time, whether or not it is
// suspended yet: bluetoothd's assertion keeps it running that long.
func (s *Suspender) Wake(pid int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pid != pid {
		return
	}
	now := time.Now()
	if s.stopped {
		log.Printf("suspend: waking %s for a Bluetooth event", s.bundle)
	}
	s.wakeUntil = now.Add(bluetoothWake)
	s.apply(now)
}

func (s *Suspender) Release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resume()
}

func (s *Suspender) apply(now time.Time) {
	if s.pid == 0 {
		return
	}
	if s.suspended && !now.Before(s.wakeUntil) {
		if !s.stopped {
			if err := syscall.Kill(s.pid, syscall.SIGSTOP); err != nil {
				log.Printf("suspend: stopping %d: %v", s.pid, err)
				return
			}
			s.stopped = true
			log.Printf("suspend: %s suspended", s.bundle)
		}
	} else {
		s.resume()
	}
}

func (s *Suspender) resume() {
	if s.stopped && s.pid != 0 {
		syscall.Kill(s.pid, syscall.SIGCONT)
		s.stopped = false
	}
}

// runningboard's state for a process, as SpringBoard logs it:
// "Received state update for 48134 (app<…>, running-suspended-NotVisible".
var stateUpdate = regexp.MustCompile(`Received state update for (\d+) \(.*?, (running-[a-z]+)`)

func (s *Suspender) watch() {
	for {
		cmd := exec.Command("xcrun", "simctl", "spawn", s.simulator, "log", "stream", "--style", "ndjson",
			"--predicate", `process == "SpringBoard" AND eventMessage CONTAINS "Received state update for"`)
		out, err := cmd.StdoutPipe()
		if err == nil {
			err = cmd.Start()
		}
		if err != nil {
			log.Printf("suspend: following runningboard on %s: %v", s.simulator, err)
			time.Sleep(5 * time.Second)
			continue
		}
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			var entry struct {
				EventMessage string `json:"eventMessage"`
			}
			if json.Unmarshal(sc.Bytes(), &entry) != nil {
				continue
			}
			m := stateUpdate.FindStringSubmatch(entry.EventMessage)
			if m == nil {
				continue
			}
			s.update(m[1], m[2] == "running-suspended")
		}
		cmd.Wait()
		time.Sleep(time.Second)
	}
}

func (s *Suspender) update(pid string, suspended bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pid == 0 || pid != strconv.Itoa(s.pid) || s.suspended == suspended {
		return
	}
	s.suspended = suspended
	s.apply(time.Now())
}
