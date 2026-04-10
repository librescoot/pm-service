package fsm_test

import (
	"context"
	"testing"
	"time"

	"github.com/librescoot/librefsm"
	"github.com/librescoot/pm-service/internal/fsm"
)

// mockActions implements fsm.Actions for testing
type mockActions struct {
	canEnterLowPower       bool
	vehicleNotInStandby    bool
	targetSuspend          bool
	targetHibernate        bool
	hasBlockingInhibitors  bool
	hasOnlyModemInhibitors bool
	targetNotRun           bool
}

func (m *mockActions) EnterPreSuspend(c *librefsm.Context) error        { return nil }
func (m *mockActions) EnterSuspendImminent(c *librefsm.Context) error   { return nil }
func (m *mockActions) EnterHibernateImminent(c *librefsm.Context) error { return nil }
func (m *mockActions) EnterWaitingInhibitors(c *librefsm.Context) error { return nil }
func (m *mockActions) EnterIssuingLowPower(c *librefsm.Context) error   { return nil }
func (m *mockActions) ExitIssuingLowPower(c *librefsm.Context) error    { return nil }
func (m *mockActions) CanEnterLowPowerState(c *librefsm.Context) bool   { return m.canEnterLowPower }
func (m *mockActions) HasNoBlockingInhibitors(c *librefsm.Context) bool {
	return !m.hasBlockingInhibitors
}
func (m *mockActions) HasOnlyModemInhibitors(c *librefsm.Context) bool {
	return m.hasOnlyModemInhibitors
}
func (m *mockActions) IsVehicleNotInStandby(c *librefsm.Context) bool {
	return m.vehicleNotInStandby
}
func (m *mockActions) IsTargetNotRun(c *librefsm.Context) bool               { return m.targetNotRun }
func (m *mockActions) IsBatteryNotActive(c *librefsm.Context) bool           { return true }
func (m *mockActions) IsTargetSuspend(c *librefsm.Context) bool              { return m.targetSuspend }
func (m *mockActions) IsTargetHibernate(c *librefsm.Context) bool            { return m.targetHibernate }
func (m *mockActions) IsPowerCommandHigherPriority(c *librefsm.Context) bool { return true }
func (m *mockActions) OnPreSuspendTimeout(c *librefsm.Context) error         { return nil }
func (m *mockActions) OnSuspendImminentTimeout(c *librefsm.Context) error    { return nil }
func (m *mockActions) OnInhibitorsChanged(c *librefsm.Context) error         { return nil }
func (m *mockActions) OnWakeup(c *librefsm.Context) error                    { return nil }
func (m *mockActions) OnDisableModem(c *librefsm.Context) error              { return nil }
func (m *mockActions) OnVehicleStateChanged(c *librefsm.Context) error       { return nil }
func (m *mockActions) OnVehicleLeftLowPowerState(c *librefsm.Context) error  { return nil }
func (m *mockActions) OnBatteryStateChanged(c *librefsm.Context) error       { return nil }
func (m *mockActions) OnPowerCommand(c *librefsm.Context) error              { return nil }
func (m *mockActions) PublishState(state string) error                       { return nil }
func (m *mockActions) PublishWakeupSource(reason string) error               { return nil }

// TestExplicitSuspendSkipsPreDelay verifies that an explicit suspend command
// goes directly to SuspendImminent (skipping the pre-suspend delay).
func TestExplicitSuspendSkipsPreDelay(t *testing.T) {
	actions := &mockActions{
		canEnterLowPower: true,
		targetSuspend:    true,
	}

	def := fsm.NewDefinition(actions, 100*time.Millisecond, 100*time.Millisecond)
	machine, err := def.Build()
	if err != nil {
		t.Fatalf("Failed to build FSM: %v", err)
	}

	ctx := context.Background()
	if err := machine.Start(ctx); err != nil {
		t.Fatalf("Failed to start FSM: %v", err)
	}
	defer machine.Stop()

	machine.Send(librefsm.Event{ID: fsm.EvPowerSuspend})
	time.Sleep(10 * time.Millisecond)
	if !machine.IsInState(fsm.StateSuspendImminent) {
		t.Errorf("Expected StateSuspendImminent (commands skip pre-delay), got %v", machine.CurrentState())
	}

	// Battery active still cancels suspend
	machine.Send(librefsm.Event{ID: fsm.EvBatteryBecameActive})
	time.Sleep(10 * time.Millisecond)
	if !machine.IsInState(fsm.StateRunning) {
		t.Errorf("Expected StateRunning after battery active, got %v", machine.CurrentState())
	}
}

// TestExplicitHibernateSkipsPreDelay verifies hibernate command goes directly
// to HibernateImminent.
func TestExplicitHibernateSkipsPreDelay(t *testing.T) {
	actions := &mockActions{
		canEnterLowPower: true,
		targetHibernate:  true,
	}

	def := fsm.NewDefinition(actions, 100*time.Millisecond, 100*time.Millisecond)
	machine, err := def.Build()
	if err != nil {
		t.Fatalf("Failed to build FSM: %v", err)
	}

	ctx := context.Background()
	if err := machine.Start(ctx); err != nil {
		t.Fatalf("Failed to start FSM: %v", err)
	}
	defer machine.Stop()

	machine.Send(librefsm.Event{ID: fsm.EvPowerHibernate})
	time.Sleep(10 * time.Millisecond)
	if !machine.IsInState(fsm.StateHibernateImminent) {
		t.Errorf("Expected StateHibernateImminent, got %v", machine.CurrentState())
	}

	// Battery active does NOT cancel hibernate
	machine.Send(librefsm.Event{ID: fsm.EvBatteryBecameActive})
	time.Sleep(10 * time.Millisecond)
	if !machine.IsInState(fsm.StateHibernateImminent) {
		t.Errorf("Expected StateHibernateImminent (battery shouldn't affect hibernate), got %v", machine.CurrentState())
	}
}

// TestNaturalSuspendUsesPreDelay verifies that the natural suspend path
// (vehicle enters stand-by with target=suspend) goes through PreSuspend.
func TestNaturalSuspendUsesPreDelay(t *testing.T) {
	actions := &mockActions{
		canEnterLowPower: true,
		targetSuspend:    true,
	}

	def := fsm.NewDefinition(actions, 100*time.Millisecond, 100*time.Millisecond)
	machine, err := def.Build()
	if err != nil {
		t.Fatalf("Failed to build FSM: %v", err)
	}

	ctx := context.Background()
	if err := machine.Start(ctx); err != nil {
		t.Fatalf("Failed to start FSM: %v", err)
	}
	defer machine.Stop()

	machine.Send(librefsm.Event{ID: fsm.EvVehicleStateChanged})
	time.Sleep(10 * time.Millisecond)
	if !machine.IsInState(fsm.StatePreSuspend) {
		t.Errorf("Expected StatePreSuspend (natural suspend uses pre-delay), got %v", machine.CurrentState())
	}
}

// TestNaturalHibernateSkipsPreDelay verifies that vehicle state change with
// hibernate target goes directly to HibernateImminent (no pre-delay).
func TestNaturalHibernateSkipsPreDelay(t *testing.T) {
	actions := &mockActions{
		canEnterLowPower: true,
		targetHibernate:  true,
	}

	def := fsm.NewDefinition(actions, 100*time.Millisecond, 100*time.Millisecond)
	machine, err := def.Build()
	if err != nil {
		t.Fatalf("Failed to build FSM: %v", err)
	}

	ctx := context.Background()
	if err := machine.Start(ctx); err != nil {
		t.Fatalf("Failed to start FSM: %v", err)
	}
	defer machine.Stop()

	machine.Send(librefsm.Event{ID: fsm.EvVehicleStateChanged})
	time.Sleep(10 * time.Millisecond)
	if !machine.IsInState(fsm.StateHibernateImminent) {
		t.Errorf("Expected StateHibernateImminent (hibernate skips pre-delay), got %v", machine.CurrentState())
	}
}

// TestBatteryInactiveTriggersNaturalSuspend: battery-inactive while in stand-by
// with target=suspend enters the pre-delay natural path.
func TestBatteryInactiveTriggersNaturalSuspend(t *testing.T) {
	actions := &mockActions{
		canEnterLowPower: true,
		targetSuspend:    true,
	}

	def := fsm.NewDefinition(actions, 100*time.Millisecond, 100*time.Millisecond)
	machine, err := def.Build()
	if err != nil {
		t.Fatalf("Failed to build FSM: %v", err)
	}

	ctx := context.Background()
	if err := machine.Start(ctx); err != nil {
		t.Fatalf("Failed to start FSM: %v", err)
	}
	defer machine.Stop()

	machine.Send(librefsm.Event{ID: fsm.EvBatteryBecameInactive})
	time.Sleep(10 * time.Millisecond)
	if !machine.IsInState(fsm.StatePreSuspend) {
		t.Errorf("Expected StatePreSuspend, got %v", machine.CurrentState())
	}
}

// TestBatteryInactiveIgnoredForHibernate: battery events do not start the
// hibernate path.
func TestBatteryInactiveIgnoredForHibernate(t *testing.T) {
	actions := &mockActions{
		canEnterLowPower: true,
		targetHibernate:  true,
	}

	def := fsm.NewDefinition(actions, 100*time.Millisecond, 100*time.Millisecond)
	machine, err := def.Build()
	if err != nil {
		t.Fatalf("Failed to build FSM: %v", err)
	}

	ctx := context.Background()
	if err := machine.Start(ctx); err != nil {
		t.Fatalf("Failed to start FSM: %v", err)
	}
	defer machine.Stop()

	machine.Send(librefsm.Event{ID: fsm.EvBatteryBecameInactive})
	time.Sleep(10 * time.Millisecond)
	if !machine.IsInState(fsm.StateRunning) {
		t.Errorf("Expected StateRunning, got %v", machine.CurrentState())
	}
}

// TestSuspendImminentCancelOnBattery: battery active in SuspendImminent cancels.
func TestSuspendImminentCancelOnBattery(t *testing.T) {
	actions := &mockActions{
		canEnterLowPower: true,
		targetSuspend:    true,
	}

	def := fsm.NewDefinition(actions, 50*time.Millisecond, 50*time.Millisecond)
	machine, err := def.Build()
	if err != nil {
		t.Fatalf("Failed to build FSM: %v", err)
	}

	ctx := context.Background()
	if err := machine.Start(ctx); err != nil {
		t.Fatalf("Failed to start FSM: %v", err)
	}
	defer machine.Stop()

	// Explicit suspend → SuspendImminent directly
	machine.Send(librefsm.Event{ID: fsm.EvPowerSuspend})
	time.Sleep(10 * time.Millisecond)
	if !machine.IsInState(fsm.StateSuspendImminent) {
		t.Errorf("Expected StateSuspendImminent, got %v", machine.CurrentState())
	}

	machine.Send(librefsm.Event{ID: fsm.EvBatteryBecameActive})
	time.Sleep(10 * time.Millisecond)
	if !machine.IsInState(fsm.StateRunning) {
		t.Errorf("Expected StateRunning after battery active, got %v", machine.CurrentState())
	}
}

// TestWakeupRoutingBasedOnTarget is a placeholder for wakeup routing logic —
// driving from StateSuspended requires more setup than this mock provides.
func TestWakeupRoutingBasedOnTarget(t *testing.T) {
	tests := []struct {
		name            string
		targetSuspend   bool
		targetHibernate bool
	}{
		{"suspend target", true, false},
		{"hibernate target", false, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actions := &mockActions{
				canEnterLowPower: true,
				targetSuspend:    tt.targetSuspend,
				targetHibernate:  tt.targetHibernate,
			}

			def := fsm.NewDefinition(actions, 100*time.Millisecond, 100*time.Millisecond)
			machine, err := def.Build()
			if err != nil {
				t.Fatalf("Failed to build FSM: %v", err)
			}

			ctx := context.Background()
			if err := machine.Start(ctx); err != nil {
				t.Fatalf("Failed to start FSM: %v", err)
			}
			defer machine.Stop()

			machine.Send(librefsm.Event{ID: fsm.EvWakeup})
			time.Sleep(10 * time.Millisecond)
		})
	}
}

// TestHibernateCommandInHibernate: a second hibernate command from Running
// state in hibernate goes straight to HibernateImminent (priority passes).
func TestHibernateCommandInHibernate(t *testing.T) {
	actions := &mockActions{
		canEnterLowPower: true,
		targetHibernate:  true,
	}

	def := fsm.NewDefinition(actions, 100*time.Millisecond, 100*time.Millisecond)
	machine, err := def.Build()
	if err != nil {
		t.Fatalf("Failed to build FSM: %v", err)
	}

	ctx := context.Background()
	if err := machine.Start(ctx); err != nil {
		t.Fatalf("Failed to start FSM: %v", err)
	}
	defer machine.Stop()

	machine.Send(librefsm.Event{ID: fsm.EvPowerHibernate})
	time.Sleep(10 * time.Millisecond)
	if !machine.IsInState(fsm.StateHibernateImminent) {
		t.Errorf("Expected StateHibernateImminent, got %v", machine.CurrentState())
	}
}

// TestRapidStateChanges: a sequence of commands collapses back to Running
// when the last command is run.
func TestRapidStateChanges(t *testing.T) {
	actions := &mockActions{
		canEnterLowPower: true,
		targetSuspend:    true,
		targetHibernate:  true,
	}

	def := fsm.NewDefinition(actions, 100*time.Millisecond, 100*time.Millisecond)
	machine, err := def.Build()
	if err != nil {
		t.Fatalf("Failed to build FSM: %v", err)
	}

	ctx := context.Background()
	if err := machine.Start(ctx); err != nil {
		t.Fatalf("Failed to start FSM: %v", err)
	}
	defer machine.Stop()

	machine.Send(librefsm.Event{ID: fsm.EvPowerSuspend})
	time.Sleep(5 * time.Millisecond)
	machine.Send(librefsm.Event{ID: fsm.EvPowerRun})
	time.Sleep(5 * time.Millisecond)
	machine.Send(librefsm.Event{ID: fsm.EvPowerSuspend})
	time.Sleep(5 * time.Millisecond)
	machine.Send(librefsm.Event{ID: fsm.EvPowerHibernate})
	time.Sleep(5 * time.Millisecond)
	machine.Send(librefsm.Event{ID: fsm.EvPowerRun})
	time.Sleep(10 * time.Millisecond)

	if !machine.IsInState(fsm.StateRunning) {
		t.Errorf("Expected StateRunning after rapid changes, got %v", machine.CurrentState())
	}
}

// TestNaturalSuspendTimeoutProgression: PreSuspend → SuspendImminent → WaitingInhibitors.
func TestNaturalSuspendTimeoutProgression(t *testing.T) {
	actions := &mockActions{
		canEnterLowPower: true,
		targetSuspend:    true,
	}

	def := fsm.NewDefinition(actions, 50*time.Millisecond, 50*time.Millisecond)
	machine, err := def.Build()
	if err != nil {
		t.Fatalf("Failed to build FSM: %v", err)
	}

	ctx := context.Background()
	if err := machine.Start(ctx); err != nil {
		t.Fatalf("Failed to start FSM: %v", err)
	}
	defer machine.Stop()

	// Natural path via EvVehicleStateChanged
	machine.Send(librefsm.Event{ID: fsm.EvVehicleStateChanged})
	time.Sleep(10 * time.Millisecond)
	if !machine.IsInState(fsm.StatePreSuspend) {
		t.Errorf("Expected StatePreSuspend, got %v", machine.CurrentState())
	}

	time.Sleep(60 * time.Millisecond)
	if !machine.IsInState(fsm.StateSuspendImminent) {
		t.Errorf("Expected StateSuspendImminent after timeout, got %v", machine.CurrentState())
	}

	time.Sleep(60 * time.Millisecond)
	if !machine.IsInState(fsm.StateWaitingInhibitors) {
		t.Errorf("Expected StateWaitingInhibitors after timeout, got %v", machine.CurrentState())
	}
}

// TestVehicleLeavingStandbyCancel: leaving stand-by cancels from any
// low-power waiting state.
func TestVehicleLeavingStandbyCancel(t *testing.T) {
	actions := &mockActions{
		canEnterLowPower:    true,
		targetSuspend:       true,
		vehicleNotInStandby: true,
	}

	def := fsm.NewDefinition(actions, 100*time.Millisecond, 100*time.Millisecond)
	machine, err := def.Build()
	if err != nil {
		t.Fatalf("Failed to build FSM: %v", err)
	}

	ctx := context.Background()
	if err := machine.Start(ctx); err != nil {
		t.Fatalf("Failed to start FSM: %v", err)
	}
	defer machine.Stop()

	machine.Send(librefsm.Event{ID: fsm.EvPowerSuspend})
	time.Sleep(10 * time.Millisecond)
	if !machine.IsInState(fsm.StateSuspendImminent) {
		t.Errorf("Expected StateSuspendImminent, got %v", machine.CurrentState())
	}

	machine.Send(librefsm.Event{ID: fsm.EvVehicleStateChanged})
	time.Sleep(10 * time.Millisecond)
	if !machine.IsInState(fsm.StateRunning) {
		t.Errorf("Expected StateRunning after vehicle left stand-by, got %v", machine.CurrentState())
	}
}
