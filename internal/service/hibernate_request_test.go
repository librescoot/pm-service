package service

import (
	"context"
	"io"
	"log"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/librescoot/librefsm"
	"github.com/librescoot/pm-service/internal/config"
	"github.com/librescoot/pm-service/internal/fsm"
	"github.com/librescoot/pm-service/internal/hibernation"
	"github.com/librescoot/pm-service/internal/inhibitor"
	redis_ipc "github.com/librescoot/redis-ipc"
)

func hibernateRequestService(t *testing.T, state string) *Service {
	t.Helper()
	mr := miniredis.RunT(t)
	client, err := redis_ipc.New(redis_ipc.WithURL(mr.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	logger := log.New(io.Discard, "", 0)
	manager, err := inhibitor.NewManager(logger, filepath.Join(t.TempDir(), "inhibitor.sock"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { manager.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := &Service{
		config: &config.Config{DefaultState: fsm.TargetRun, DryRun: true, WakeTimerMaxSeconds: 604800, WakeTimerAckTimeout: 20 * time.Millisecond},
		logger: logger, redis: client, powerManagerPub: client.NewHashPublisher("power-manager"),
		inhibitorManager: manager, ctx: ctx,
		fsmData:       &fsm.FSMData{TargetPowerState: fsm.TargetRun, VehicleState: state},
		wakeTimerAcks: make(chan uint32, 1), modemCmdFn: func(string) error { return nil },
	}
	s.hibernationTimer = hibernation.NewTimer(ctx, logger, 0, nil)
	t.Cleanup(s.hibernationTimer.Close)
	if err := client.HSet("vehicle", "state", state); err != nil {
		t.Fatal(err)
	}
	s.machine, err = fsm.NewDefinition(s, time.Hour, time.Hour).Build(librefsm.WithData(s.fsmData), librefsm.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.machine.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.machine.Stop(); s.stopHibernatePreparationTimer() })
	return s
}

func flushHibernateEvents(t *testing.T, s *Service) {
	t.Helper()
	if err := s.machine.SendSync(librefsm.Event{ID: "test-barrier"}); err != nil {
		t.Fatal(err)
	}
}

func requestHibernate(t *testing.T, s *Service, cmd string) {
	t.Helper()
	if err := s.onPowerCommand(cmd); err != nil {
		t.Fatal(err)
	}
	flushHibernateEvents(t, s)
	flushHibernateEvents(t, s)
}

func hibernateState(t *testing.T, s *Service, state string) {
	t.Helper()
	if err := s.redis.HSet("vehicle", "state", state); err != nil {
		t.Fatal(err)
	}
	if err := s.machine.SendSync(librefsm.Event{ID: fsm.EvVehicleStateChanged, Payload: fsm.VehicleStatePayload{State: state}}); err != nil {
		t.Fatal(err)
	}
}

func TestExplicitHibernateAdmissionMatrix(t *testing.T) {
	for _, command := range []string{"hibernate", "hibernate-manual", "hibernate-for:28800"} {
		for _, state := range []string{"stand-by", "parked", "ready-to-drive", "shutting-down", "waiting-seatbox", "hop-on", "waiting-hibernation", ""} {
			t.Run(command+"/"+state, func(t *testing.T) {
				s := hibernateRequestService(t, state)
				requestHibernate(t, s, command)
				allowed := state == "parked" || state == "stand-by"
				if allowed != (s.fsmData.HibernateRequestID != "") {
					t.Fatalf("admission: id=%q target=%q", s.fsmData.HibernateRequestID, s.fsmData.TargetPowerState)
				}
				if !allowed {
					if s.fsmData.TargetPowerState != fsm.TargetRun || s.fsmData.HibernateForWakeSeconds != 0 {
						t.Fatal("rejected request left power intent")
					}
					hibernateState(t, s, "stand-by")
					if s.machine.CurrentState() != fsm.StateRunning {
						t.Fatal("rejected request fired on later standby")
					}
					return
				}
				if state == "parked" {
					prep, err := s.redis.LPop("scooter:state")
					if err != nil || prep != "prepare-hibernate:"+s.fsmData.HibernateRequestID {
						t.Fatalf("preparation=%q err=%v", prep, err)
					}
					if s.machine.CurrentState() != fsm.StateRunning {
						t.Fatal("entered low power before vehicle shutdown")
					}
					hibernateState(t, s, "shutting-down")
					hibernateState(t, s, "stand-by")
				}
				if s.machine.CurrentState() != fsm.StateLowPowerImminent {
					t.Fatalf("standby did not start power preparation: %s", s.machine.CurrentState())
				}
				if strings.HasPrefix(command, "hibernate-for:") {
					if s.fsmData.HibernateForWakeSeconds != 28800 {
						t.Fatal("duration was lost during vehicle shutdown")
					}
					seconds, _ := s.redis.HGet("power-manager", "wake-timer-seconds")
					if seconds != "28800" {
						t.Fatalf("timer=%q", seconds)
					}
				}
			})
		}
	}
}

func TestHibernatePreparationCancellationAndFailure(t *testing.T) {
	for _, action := range []string{"cancel", "interrupted", "drive", "failed", "timeout"} {
		t.Run(action, func(t *testing.T) {
			s := hibernateRequestService(t, "parked")
			requestHibernate(t, s, "hibernate-for:28800")
			id := s.fsmData.HibernateRequestID
			switch action {
			case "cancel":
				requestHibernate(t, s, "hibernate-cancel")
			case "interrupted":
				hibernateState(t, s, "shutting-down")
				hibernateState(t, s, "parked")
			case "drive":
				hibernateState(t, s, "ready-to-drive")
			case "failed":
				requestHibernate(t, s, "hibernate-preparation-failed:"+id)
			case "timeout":
				s.machine.SendSync(librefsm.Event{ID: fsm.EvHibernatePreparationFailed, Payload: fsm.HibernatePreparationResult{RequestID: id, Error: "vehicle preparation timed out"}})
			}
			if s.fsmData.HibernateRequestID != "" || s.fsmData.HibernateForWakeSeconds != 0 || s.hibernatePreparationTimer != nil {
				t.Fatal("preparation intent survived cancellation/failure")
			}
			stored, _ := s.redis.HGet("power-manager", "hibernate-request-id")
			if stored != "" {
				t.Fatal("vehicle can still execute a cancelled preparation")
			}
			hibernateState(t, s, "stand-by")
			if s.machine.CurrentState() != fsm.StateRunning {
				t.Fatal("cancelled request fired on later standby")
			}
		})
	}
}

func TestHibernateReplacementIgnoresStalePreparationResult(t *testing.T) {
	s := hibernateRequestService(t, "parked")
	requestHibernate(t, s, "hibernate-for:28800")
	old := s.fsmData.HibernateRequestID
	requestHibernate(t, s, "hibernate-for:3600")
	current := s.fsmData.HibernateRequestID
	if current == old || current == "" {
		t.Fatal("replacement did not get a fresh request id")
	}
	requestHibernate(t, s, "hibernate-preparation-failed:"+old)
	if s.fsmData.HibernateRequestID != current || s.fsmData.HibernateForWakeSeconds != 3600 {
		t.Fatal("stale failure cancelled replacement")
	}
	requestHibernate(t, s, "hibernate")
	if s.fsmData.HibernateForWakeSeconds != 0 || s.fsmData.TargetPowerState != fsm.TargetHibernateManual {
		t.Fatal("explicit ordinary sleep did not replace timed intent")
	}
}

func TestHibernatePreparationRespectsRealBlocks(t *testing.T) {
	for _, kind := range []inhibitor.InhibitorType{inhibitor.TypeBlock, inhibitor.TypeSuspendOnly} {
		t.Run(string(kind), func(t *testing.T) {
			s := hibernateRequestService(t, "parked")
			s.inhibitorManager.AddInhibitor("installer", "power", "test", kind)
			requestHibernate(t, s, "hibernate-for:3600")
			if (s.fsmData.HibernateRequestID != "") != (kind == inhibitor.TypeSuspendOnly) {
				t.Fatal("incorrect hibernation inhibitor classification")
			}
		})
	}
}

func TestModemBlockDoesNotPreventVehiclePreparation(t *testing.T) {
	s := hibernateRequestService(t, "parked")
	s.inhibitorManager.AddInhibitor("librescoot-modem", "power", "modem active", inhibitor.TypeBlock)
	requestHibernate(t, s, "hibernate-for:3600")
	if s.fsmData.HibernateRequestID == "" {
		t.Fatal("modem's normal shutdown handshake prevented vehicle preparation")
	}
}

func TestAutomaticHibernateDoesNotPrepareVehicle(t *testing.T) {
	for _, event := range []librefsm.EventID{fsm.EvPowerHibernate, fsm.EvPowerHibernateFor, fsm.EvHibernationTimerExpired} {
		t.Run(string(event), func(t *testing.T) {
			s := hibernateRequestService(t, "parked")
			s.machine.SendSync(librefsm.Event{ID: event, Payload: fsm.PowerCommandPayload{TargetState: fsm.TargetHibernateFor, WakeSeconds: 3600}})
			if s.fsmData.HibernateRequestID != "" || s.machine.CurrentState() != fsm.StateRunning {
				t.Fatal("automatic request initiated locking")
			}
			if cmd, _ := s.redis.LPop("scooter:state"); cmd != "" {
				t.Fatalf("automatic vehicle command: %q", cmd)
			}
		})
	}
}

func TestTimedHibernateAckFailureRemainsAwake(t *testing.T) {
	for _, ack := range []string{"timeout", "0", "1800", "3600"} {
		t.Run(ack, func(t *testing.T) {
			s := hibernateRequestService(t, "stand-by")
			requestHibernate(t, s, "hibernate-for:3600")
			if ack != "timeout" {
				s.onWakeTimerAckSeconds(ack)
			}
			s.machine.SendSync(librefsm.Event{ID: fsm.EvSuspendImminentTimeout})
			flushHibernateEvents(t, s)
			flushHibernateEvents(t, s)
			if ack == "3600" {
				if s.machine.CurrentState() != fsm.StateIssuingLowPower || !s.wakeTimerArmed {
					t.Fatal("confirmed wake timer did not permit poweroff")
				}
			} else {
				if s.machine.CurrentState() != fsm.StateRunning || s.fsmData.HibernateForWakeSeconds != 0 {
					t.Fatal("ACK failure did not return to running")
				}
				status, _ := s.redis.HGet("power-manager", "hibernate-status")
				if status != "failed" {
					t.Fatalf("failure status=%q", status)
				}
			}
		})
	}
}
