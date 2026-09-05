package service

import (
	"errors"
	"io"
	"log"
	"testing"

	"github.com/librescoot/librefsm"
	"github.com/librescoot/pm-service/internal/config"
	"github.com/librescoot/pm-service/internal/fsm"
)

func newModemCommandService(t *testing.T) (*Service, *[]string) {
	t.Helper()
	var pushed []string
	s := &Service{
		logger:  log.New(io.Discard, "", 0),
		config:  &config.Config{DefaultState: "suspend"},
		fsmData: &fsm.FSMData{},
		modemCmdFn: func(cmd string) error {
			pushed = append(pushed, cmd)
			return nil
		},
	}
	return s, &pushed
}

func TestEnableModemOnlyPushesWhenDisabled(t *testing.T) {
	s, pushed := newModemCommandService(t)

	s.enableModem()
	if len(*pushed) != 0 {
		t.Fatalf("enable pushed with the modem already on: %v", *pushed)
	}

	s.disableModem()
	s.enableModem()
	if got := *pushed; len(got) != 2 || got[0] != "disable" || got[1] != "enable" {
		t.Fatalf("expected disable then enable, got %v", got)
	}
	if s.fsmData.ModemDisabled {
		t.Fatal("ModemDisabled still set after enableModem")
	}

	s.enableModem()
	if len(*pushed) != 2 {
		t.Fatalf("redundant enable pushed: %v", *pushed)
	}
}

func TestEnableModemRetriesAfterPushFailure(t *testing.T) {
	s, _ := newModemCommandService(t)
	s.fsmData.ModemDisabled = true
	attempts := 0
	s.modemCmdFn = func(string) error {
		attempts++
		if attempts == 1 {
			return errors.New("push failed")
		}
		return nil
	}

	s.enableModem()
	if !s.fsmData.ModemDisabled {
		t.Fatal("failed push cleared ModemDisabled")
	}
	s.enableModem()
	if s.fsmData.ModemDisabled || attempts != 2 {
		t.Fatalf("retry did not restore modem: disabled=%v attempts=%d", s.fsmData.ModemDisabled, attempts)
	}
}

func TestPowerRunReEnablesModem(t *testing.T) {
	s, pushed := newModemCommandService(t)
	s.disableModem()

	c := &librefsm.Context{Event: &librefsm.Event{
		Payload: fsm.PowerCommandPayload{TargetState: fsm.TargetRun},
	}}
	if err := s.OnPowerCommand(c); err != nil {
		t.Fatalf("OnPowerCommand: %v", err)
	}
	if got := *pushed; len(got) != 2 || got[1] != "enable" {
		t.Fatalf("expected disable then enable, got %v", got)
	}
}

func TestVehicleLeftLowPowerStateReEnablesModem(t *testing.T) {
	s, pushed := newModemCommandService(t)
	s.disableModem()

	c := &librefsm.Context{Event: &librefsm.Event{
		ID:      fsm.EvVehicleStateChanged,
		Payload: fsm.VehicleStatePayload{State: "parked"},
	}}
	if err := s.OnVehicleLeftLowPowerState(c); err != nil {
		t.Fatalf("OnVehicleLeftLowPowerState: %v", err)
	}

	if got := *pushed; len(got) != 2 || got[1] != "enable" {
		t.Fatalf("expected the modem back on, pushes: %v", got)
	}
}

func TestVehicleStateChangedOutOfStandbyReEnablesModem(t *testing.T) {
	s, pushed := newModemCommandService(t)
	s.fsmData.VehicleState = "stand-by"
	s.disableModem()

	c := &librefsm.Context{Event: &librefsm.Event{
		ID:      fsm.EvVehicleStateChanged,
		Payload: fsm.VehicleStatePayload{State: "parked"},
	}}
	if err := s.OnVehicleStateChanged(c); err != nil {
		t.Fatalf("OnVehicleStateChanged: %v", err)
	}

	if got := *pushed; len(got) != 2 || got[1] != "enable" {
		t.Fatalf("expected the modem back on, pushes: %v", got)
	}
}

func TestClearedFlagSuppressesEnable(t *testing.T) {
	s, pushed := newModemCommandService(t)
	s.disableModem()

	s.fsmData.ModemDisabled = false

	s.enableModem()
	if got := *pushed; len(got) != 1 {
		t.Fatalf("resume path pushed an enable: %v", got)
	}
}
