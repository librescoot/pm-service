package service

import (
	"io"
	"log"
	"testing"
	"time"

	"github.com/librescoot/librefsm"
	"github.com/librescoot/pm-service/internal/config"
	"github.com/librescoot/pm-service/internal/fsm"
)

// newHibernateForService builds the minimum Service needed to exercise the
// wake-timer ACK gate in EnterIssuingLowPower. DryRun makes the action return
// right after the gate, before it reaches the systemd and inhibitor plumbing.
func newHibernateForService(t *testing.T) *Service {
	t.Helper()
	s := &Service{
		logger: log.New(io.Discard, "", 0),
		config: &config.Config{
			DryRun:              true,
			WakeTimerAckTimeout: 50 * time.Millisecond,
		},
		wakeTimerAcks: make(chan uint32, 1),
		fsmData:       &fsm.FSMData{},
	}
	s.fsmData.TargetPowerState = fsm.TargetHibernateFor
	s.fsmData.HibernateForWakeSeconds = 3600
	return s
}

// A late blocking inhibitor bounces the FSM out of issuing-low-power and back
// in, so the action runs twice per hibernate-for round. The ACK arrives once;
// the second pass must not wait for another one.
func TestWakeTimerAckLatchSurvivesReentry(t *testing.T) {
	s := newHibernateForService(t)
	if err := s.onWakeTimerAckSeconds("3600"); err != nil {
		t.Fatalf("onWakeTimerAckSeconds: %v", err)
	}

	if err := s.EnterIssuingLowPower(nil); err != nil {
		t.Fatalf("first EnterIssuingLowPower: %v", err)
	}
	if !s.wakeTimerArmed {
		t.Fatal("ACK was not latched on the first pass")
	}

	// No ACK left in the channel. Without the latch this blocks for the full
	// timeout and then takes the abort path, which sends EvPowerRun on the nil
	// context we passed in.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("second pass took the abort path instead of using the latched ACK: %v", r)
		}
	}()
	start := time.Now()
	if err := s.EnterIssuingLowPower(nil); err != nil {
		t.Fatalf("second EnterIssuingLowPower: %v", err)
	}
	if elapsed := time.Since(start); elapsed >= s.config.WakeTimerAckTimeout {
		t.Fatalf("second pass waited %v for an ACK it already had", elapsed)
	}
}

func TestWakeTimerWaitIgnoresDisarmEcho(t *testing.T) {
	s := newHibernateForService(t)
	s.onWakeTimerAckSeconds("0")
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(5 * time.Millisecond)
		s.onWakeTimerAckSeconds("3600")
	}()
	if !s.waitForWakeTimer(time.Second) {
		t.Fatal("a disarm echo prevented confirmation of the requested duration")
	}
	<-done
}

// A power command with a different target must not carry an ACK from another
// hibernate-for round into the next one.
func TestWakeTimerLatchClearedByNewTarget(t *testing.T) {
	s := newHibernateForService(t)
	s.wakeTimerArmed = true

	c := &librefsm.Context{Event: &librefsm.Event{
		ID:      fsm.EvPowerHibernate,
		Payload: fsm.PowerCommandPayload{TargetState: fsm.TargetHibernate},
	}}
	if err := s.OnPowerCommand(c); err != nil {
		t.Fatalf("OnPowerCommand: %v", err)
	}
	if s.wakeTimerArmed {
		t.Fatal("latch survived a change of power target")
	}
}
