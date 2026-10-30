package suspend

import (
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestSuspenderFollowsRunningboardAndWakesForBluetooth(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		syscall.Kill(cmd.Process.Pid, syscall.SIGCONT)
		cmd.Process.Kill()
		cmd.Wait()
	}()
	pid := cmd.Process.Pid
	s := &Suspender{}
	s.Attach(pid, "com.example.loop")
	stopped := func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.stopped
	}

	s.update(strconv.Itoa(pid), true)
	if !stopped() {
		t.Fatal("not stopped once runningboard suspended it")
	}
	s.Wake(pid)
	if stopped() {
		t.Fatal("still stopped after a Bluetooth event")
	}
	s.mu.Lock()
	s.apply(time.Now().Add(bluetoothWake + time.Second))
	s.mu.Unlock()
	if !stopped() {
		t.Fatal("not stopped again once the event's run time ended")
	}
	s.update(strconv.Itoa(pid), false)
	if stopped() {
		t.Fatal("still stopped once runningboard let it run")
	}

	// An event's run time outlasts runningboard's verdicts in the meantime.
	s.Wake(pid)
	s.update(strconv.Itoa(pid), true)
	s.update(strconv.Itoa(pid), false)
	s.update(strconv.Itoa(pid), true)
	if stopped() {
		t.Fatal("stopped within a Bluetooth event's run time")
	}
	s.Detach(pid)
	if stopped() {
		t.Fatal("left stopped after detaching")
	}
}
