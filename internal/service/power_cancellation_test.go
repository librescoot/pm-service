package service

import (
	"fmt"
	"testing"
	"time"

	"github.com/librescoot/librefsm"
	"github.com/librescoot/pm-service/internal/fsm"
	"github.com/librescoot/pm-service/internal/inhibitor"
)

func cancellationService(t *testing.T, state librefsm.StateID, target string) (*Service, *[]string) {
	t.Helper()
	s := hibernateRequestService(t, "stand-by")
	s.machine.Stop()
	s.fsmData.TargetPowerState = target
	s.suspendWhenOnline = false
	for _, slot := range []string{"0", "1"} {
		setPowerInput(t, s, "battery:"+slot, "present", "false")
		setPowerInput(t, s, "battery:"+slot, "state", "idle")
		setPowerInput(t, s, "battery:"+slot, "charge", "0")
	}
	var pushed []string
	s.modemCmdFn = func(cmd string) error { pushed = append(pushed, cmd); return nil }
	s.inhibitorManager.AddInhibitor("librescoot-modem", "modem", "powered", inhibitor.TypeBlock)
	var err error
	s.machine, err = fsm.NewDefinition(s, time.Hour, time.Hour).Initial(state).Build()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.machine.Start(s.ctx); err != nil {
		t.Fatal(err)
	}
	return s, &pushed
}

func setPowerInput(t *testing.T, s *Service, hash, field, value string) {
	t.Helper()
	if err := s.redis.HSet(hash, field, value); err != nil {
		t.Fatal(err)
	}
}

func sendPowerEvent(t *testing.T, s *Service, event librefsm.Event) {
	t.Helper()
	if err := s.machine.SendSync(event); err != nil {
		t.Fatal(err)
	}
	flushHibernateEvents(t, s)
	flushHibernateEvents(t, s)
}

func assertModemRestored(t *testing.T, s *Service, pushed []string) {
	t.Helper()
	if s.fsmData.ModemDisabled || len(pushed) != 2 || pushed[0] != "disable" || pushed[1] != "enable" {
		t.Fatalf("modem disabled=%v commands=%v; want disable then enable", s.fsmData.ModemDisabled, pushed)
	}
}

func TestBatteryActivationRestoresModemAfterSuspendCancellation(t *testing.T) {
	for _, slot := range []string{"0", "1"} {
		t.Run(slot, func(t *testing.T) {
			s, pushed := cancellationService(t, fsm.StateWaitingInhibitors, fsm.TargetSuspend)
			setPowerInput(t, s, "battery:"+slot, "state", "active")
			sendPowerEvent(t, s, librefsm.Event{ID: fsm.EvBatteryBecameActive, Payload: fsm.BatteryStatePayload{Slot: slot, State: "active"}})
			if !s.machine.IsInState(fsm.StateRunning) || s.fsmData.BatteryState != "active" {
				t.Fatalf("state=%v battery=%s", s.machine.CurrentState(), s.fsmData.BatteryState)
			}
			assertModemRestored(t, s, *pushed)
		})
	}
}

func TestLateSuspendBlockerCancelsBeforeIssuing(t *testing.T) {
	for _, blocker := range []string{"idle-pack-inserted", "remote-provider-connected", "online-policy-changed"} {
		for _, event := range []librefsm.EventID{fsm.EvInhibitorsChanged, fsm.EvInhibitorWaitTimeout} {
			t.Run(blocker+"/"+string(event), func(t *testing.T) {
				s, pushed := cancellationService(t, fsm.StateWaitingInhibitors, fsm.TargetSuspend)
				switch blocker {
				case "idle-pack-inserted":
					setPowerInput(t, s, "battery:1", "present", "true")
				case "online-policy-changed":
					s.onSuspendWhenOnlineSetting("true")
					flushHibernateEvents(t, s)
					setPowerInput(t, s, "remote-access", "wireguard", "connected")
					s.onSuspendWhenOnlineSetting("false")
				default:
					setPowerInput(t, s, "remote-access", "wireguard", "connected")
				}
				sendPowerEvent(t, s, librefsm.Event{ID: event})
				if !s.machine.IsInState(fsm.StateRunning) {
					t.Fatalf("late blocker did not cancel: %v", s.machine.CurrentState())
				}
				assertModemRestored(t, s, *pushed)
			})
		}
	}
}

func TestRecoveredSupplyCancelsOnlyAutomaticHibernate(t *testing.T) {
	for _, automatic := range []bool{true, false} {
		for _, fallback := range []string{fsm.TargetRun, fsm.TargetSuspend} {
			name := fallback + "/explicit"
			if automatic {
				name = fallback + "/automatic"
			}
			t.Run(name, func(t *testing.T) {
				s, pushed := cancellationService(t, fsm.StateRunning, fallback)
				s.lastDitchEnabled = true
				s.lastDitchGraceUntil = time.Now().Add(-time.Minute)
				setPowerInput(t, s, "aux-battery", "voltage", "11000")
				event := fsm.EvPowerHibernate
				if automatic {
					event = fsm.EvLastDitchCheck
				}
				sendPowerEvent(t, s, librefsm.Event{ID: event, Payload: fsm.PowerCommandPayload{TargetState: fsm.TargetHibernate}})
				if !s.machine.IsInState(fsm.StateLowPowerImminent) {
					t.Fatalf("hibernate not armed: %v", s.machine.CurrentState())
				}
				sendPowerEvent(t, s, librefsm.Event{ID: fsm.EvSuspendImminentTimeout})
				if !s.machine.IsInState(fsm.StateWaitingInhibitors) {
					t.Fatalf("not waiting: %v", s.machine.CurrentState())
				}
				setPowerInput(t, s, "battery:1", "present", "true")
				setPowerInput(t, s, "battery:1", "charge", "100")
				setPowerInput(t, s, "battery:1", "state", "active")
				s.onBatteryPresentChanged("1", "true")
				s.onBatteryChargeChanged("1", "100")
				s.onBatteryStateChanged("1", "active")
				flushHibernateEvents(t, s)
				flushHibernateEvents(t, s)
				if automatic {
					if !s.machine.IsInState(fsm.StateRunning) || s.fsmData.AutomaticLastDitch || s.fsmData.TargetPowerState != fallback {
						t.Fatalf("automatic hibernate retained: state=%v data=%+v", s.machine.CurrentState(), s.fsmData)
					}
					assertModemRestored(t, s, *pushed)
				} else {
					if !s.machine.IsInState(fsm.StateWaitingInhibitors) || s.fsmData.TargetPowerState != fsm.TargetHibernate {
						t.Fatalf("explicit hibernate cancelled: %v", s.machine.CurrentState())
					}
					if len(*pushed) != 1 || (*pushed)[0] != "disable" {
						t.Fatalf("explicit transition restored modem: %v", *pushed)
					}
				}
			})
		}
	}
}

func TestRecoveryPreservesExplicitPowerTargets(t *testing.T) {
	for _, target := range []string{fsm.TargetHibernate, fsm.TargetHibernateManual, fsm.TargetHibernateFor, fsm.TargetHibernateTimer, fsm.TargetReboot} {
		t.Run(target, func(t *testing.T) {
			s, pushed := cancellationService(t, fsm.StateWaitingInhibitors, target)
			setPowerInput(t, s, "battery:1", "present", "true")
			setPowerInput(t, s, "battery:1", "charge", "100")
			setPowerInput(t, s, "battery:1", "state", "active")
			s.onBatteryPresentChanged("1", "true")
			s.onBatteryStateChanged("1", "active")
			flushHibernateEvents(t, s)
			if !s.machine.IsInState(fsm.StateWaitingInhibitors) || s.fsmData.TargetPowerState != target || len(*pushed) != 1 || (*pushed)[0] != "disable" {
				t.Fatalf("explicit request altered: state=%v target=%s modem=%v", s.machine.CurrentState(), s.fsmData.TargetPowerState, *pushed)
			}
		})
	}
}

func TestSuspendCancellationPreservesReplacementHibernate(t *testing.T) {
	s, pushed := cancellationService(t, fsm.StateWaitingInhibitors, fsm.TargetSuspend)
	sendPowerEvent(t, s, librefsm.Event{ID: fsm.EvPowerHibernateManual, Payload: fsm.PowerCommandPayload{TargetState: fsm.TargetHibernateManual}})
	sendPowerEvent(t, s, librefsm.Event{ID: fsm.EvSuspendImminentTimeout})
	sendPowerEvent(t, s, librefsm.Event{ID: fsm.EvSuspendCancelled})
	if !s.machine.IsInState(fsm.StateWaitingInhibitors) || s.fsmData.TargetPowerState != fsm.TargetHibernateManual || len(*pushed) != 1 {
		t.Fatalf("stale suspend cancellation changed replacement request: state=%v target=%s modem=%v", s.machine.CurrentState(), s.fsmData.TargetPowerState, *pushed)
	}
}

func TestSuspendRechecksEligibilityAfterQuiesce(t *testing.T) {
	for _, blocker := range []string{"idle-pack-inserted", "remote-provider-connected"} {
		t.Run(blocker, func(t *testing.T) {
			s, pushed := cancellationService(t, fsm.StateRunning, fsm.TargetSuspend)
			s.config.DryRun = false
			s.suspendQuiesceAcks = make(chan struct{}, 1)
			s.disableModem()
			for _, block := range s.inhibitorManager.GetInhibitors() {
				s.inhibitorManager.RemoveInhibitor(block)
			}
			changed := make(chan error, 1)
			go func() {
				deadline := time.NewTimer(2 * time.Second)
				defer deadline.Stop()
				tick := time.NewTicker(time.Millisecond)
				defer tick.Stop()
				for {
					select {
					case <-s.ctx.Done():
						return
					case <-deadline.C:
						changed <- fmt.Errorf("no suspending announcement")
						return
					case <-tick.C:
						state, _ := s.redis.HGet("power-manager", "state")
						if state != "suspending" {
							continue
						}
						var err error
						if blocker == "idle-pack-inserted" {
							err = s.redis.HSet("battery:1", "present", "true")
						} else {
							err = s.redis.HSet("remote-access", "wireguard", "connected")
						}
						changed <- err
						s.suspendQuiesceAcks <- struct{}{}
						return
					}
				}
			}()
			sendPowerEvent(t, s, librefsm.Event{ID: fsm.EvPowerSuspend, Payload: fsm.PowerCommandPayload{TargetState: fsm.TargetSuspend}})
			sendPowerEvent(t, s, librefsm.Event{ID: fsm.EvSuspendImminentTimeout})
			if err := <-changed; err != nil {
				t.Fatal(err)
			}
			if !s.machine.IsInState(fsm.StateRunning) || s.fsmData.LowPowerStateIssued {
				t.Fatalf("suspend not aborted after quiesce: state=%v issued=%v", s.machine.CurrentState(), s.fsmData.LowPowerStateIssued)
			}
			assertModemRestored(t, s, *pushed)
		})
	}
}

func TestAutomaticHibernateRechecksRecoveryWithoutNotification(t *testing.T) {
	for _, phase := range []librefsm.StateID{fsm.StateLowPowerImminent, fsm.StateWaitingInhibitors} {
		t.Run(string(phase), func(t *testing.T) {
			s, _ := cancellationService(t, fsm.StateRunning, fsm.TargetSuspend)
			s.lastDitchEnabled = true
			s.lastDitchGraceUntil = time.Now().Add(-time.Minute)
			setPowerInput(t, s, "aux-battery", "voltage", "11000")
			sendPowerEvent(t, s, librefsm.Event{ID: fsm.EvLastDitchCheck, Payload: fsm.PowerCommandPayload{TargetState: fsm.TargetHibernate}})
			if phase == fsm.StateWaitingInhibitors {
				sendPowerEvent(t, s, librefsm.Event{ID: fsm.EvSuspendImminentTimeout})
			}
			setPowerInput(t, s, "battery:1", "present", "true")
			setPowerInput(t, s, "battery:1", "charge", "100")
			event := fsm.EvSuspendImminentTimeout
			if phase == fsm.StateWaitingInhibitors {
				event = fsm.EvInhibitorWaitTimeout
			}
			sendPowerEvent(t, s, librefsm.Event{ID: event})
			if !s.machine.IsInState(fsm.StateRunning) || s.fsmData.AutomaticLastDitch {
				t.Fatalf("stale emergency intent survived live recheck: state=%v", s.machine.CurrentState())
			}
		})
	}
}
