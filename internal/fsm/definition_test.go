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
	canEnterLowPower          bool
	vehicleNotInStandby       bool
	targetSuspend             bool
	targetHibernate           bool
	lastDitchTriggered        bool
	hasBlockingInhibitors     bool
	hasOnlyModemInhibitors    bool
	canProceedPastModemWait   bool
	targetNotRun              bool
	onPowerCommandCount       int
	onLastDitchTriggeredCount int
	onLastDitchWakeupCount    int
}

func (m *mockActions) EnterPreSuspend(c *librefsm.Context) error        { return nil }
func (m *mockActions) EnterSuspendImminent(c *librefsm.Context) error   { return nil }
func (m *mockActions) EnterLowPowerImminent(c *librefsm.Context) error  { return nil }
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
func (m *mockActions) CanProceedPastModemWait(c *librefsm.Context) bool {
	return m.canProceedPastModemWait
}
func (m *mockActions) IsVehicleNotInStandby(c *librefsm.Context) bool {
	return m.vehicleNotInStandby
}
func (m *mockActions) IsTargetNotRun(c *librefsm.Context) bool     { return m.targetNotRun }
func (m *mockActions) IsBatteryNotActive(c *librefsm.Context) bool { return true }
func (m *mockActions) IsTargetSuspend(c *librefsm.Context) bool    { return m.targetSuspend }
func (m *mockActions) IsTargetHibernate(c *librefsm.Context) bool  { return m.targetHibernate }
func (m *mockActions) IsLastDitchTriggered(c *librefsm.Context) bool {
	return m.lastDitchTriggered
}
func (m *mockActions) IsPowerCommandHigherPriority(c *librefsm.Context) bool { return true }
func (m *mockActions) OnPreSuspendTimeout(c *librefsm.Context) error         { return nil }
func (m *mockActions) OnSuspendImminentTimeout(c *librefsm.Context) error    { return nil }
func (m *mockActions) OnInhibitorsChanged(c *librefsm.Context) error         { return nil }
func (m *mockActions) OnWakeup(c *librefsm.Context) error                    { return nil }
func (m *mockActions) OnDisableModem(c *librefsm.Context) error              { return nil }
func (m *mockActions) OnVehicleStateChanged(c *librefsm.Context) error       { return nil }
func (m *mockActions) OnVehicleLeftLowPowerState(c *librefsm.Context) error  { return nil }
func (m *mockActions) OnBatteryStateChanged(c *librefsm.Context) error       { return nil }
func (m *mockActions) OnPowerCommand(c *librefsm.Context) error {
	m.onPowerCommandCount++
	return nil
}
func (m *mockActions) OnLastDitchTriggered(c *librefsm.Context) error {
	m.onLastDitchTriggeredCount++
	return nil
}
func (m *mockActions) OnLastDitchWakeup(c *librefsm.Context) error {
	m.onLastDitchWakeupCount++
	return nil
}
func (m *mockActions) OnDefaultStateChanged(c *librefsm.Context) error { return nil }
func (m *mockActions) PublishState(state string) error         { return nil }
func (m *mockActions) PublishWakeupSource(reason string) error { return nil }

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
// to LowPowerImminent.
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
	if !machine.IsInState(fsm.StateLowPowerImminent) {
		t.Errorf("Expected StateLowPowerImminent, got %v", machine.CurrentState())
	}

	// Battery active does NOT cancel hibernate
	machine.Send(librefsm.Event{ID: fsm.EvBatteryBecameActive})
	time.Sleep(10 * time.Millisecond)
	if !machine.IsInState(fsm.StateLowPowerImminent) {
		t.Errorf("Expected StateLowPowerImminent (battery shouldn't affect hibernate), got %v", machine.CurrentState())
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
// hibernate target goes directly to LowPowerImminent (no pre-delay).
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
	if !machine.IsInState(fsm.StateLowPowerImminent) {
		t.Errorf("Expected StateLowPowerImminent (hibernate skips pre-delay), got %v", machine.CurrentState())
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
// state in hibernate goes straight to LowPowerImminent (priority passes).
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
	if !machine.IsInState(fsm.StateLowPowerImminent) {
		t.Errorf("Expected StateLowPowerImminent, got %v", machine.CurrentState())
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

// TestPreSuspendUpgradeToHibernate: hibernate command while in PreSuspend
// should upgrade to LowPowerImminent (skipping the pre-delay).
func TestPreSuspendUpgradeToHibernate(t *testing.T) {
	actions := &mockActions{
		canEnterLowPower: true,
		targetSuspend:    true,
	}

	def := fsm.NewDefinition(actions, 200*time.Millisecond, 200*time.Millisecond)
	machine, err := def.Build()
	if err != nil {
		t.Fatalf("Failed to build FSM: %v", err)
	}

	ctx := context.Background()
	if err := machine.Start(ctx); err != nil {
		t.Fatalf("Failed to start FSM: %v", err)
	}
	defer machine.Stop()

	// Enter PreSuspend via the natural path
	machine.Send(librefsm.Event{ID: fsm.EvVehicleStateChanged})
	time.Sleep(10 * time.Millisecond)
	if !machine.IsInState(fsm.StatePreSuspend) {
		t.Fatalf("Expected StatePreSuspend, got %v", machine.CurrentState())
	}

	// Hibernate upgrade — jumps directly to LowPowerImminent
	machine.Send(librefsm.Event{ID: fsm.EvPowerHibernate})
	time.Sleep(10 * time.Millisecond)
	if !machine.IsInState(fsm.StateLowPowerImminent) {
		t.Errorf("Expected StateLowPowerImminent after hibernate upgrade, got %v", machine.CurrentState())
	}
}

// TestSuspendImminentUpgradeOnHibernationTimer: the hibernation timer firing
// while in SuspendImminent should upgrade the in-flight sequence.
func TestSuspendImminentUpgradeOnHibernationTimer(t *testing.T) {
	actions := &mockActions{
		canEnterLowPower: true,
		targetSuspend:    true,
	}

	def := fsm.NewDefinition(actions, 200*time.Millisecond, 200*time.Millisecond)
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
		t.Fatalf("Expected StateSuspendImminent, got %v", machine.CurrentState())
	}

	// Hibernation timer fires — should upgrade to LowPowerImminent
	machine.Send(librefsm.Event{ID: fsm.EvHibernationTimerExpired})
	time.Sleep(10 * time.Millisecond)
	if !machine.IsInState(fsm.StateLowPowerImminent) {
		t.Errorf("Expected StateLowPowerImminent after timer upgrade, got %v", machine.CurrentState())
	}
}

// TestLowPowerImminentSelfUpgrade: hibernate-manual while in LowPowerImminent
// is a valid priority upgrade and re-enters the imminent state.
func TestLowPowerImminentSelfUpgrade(t *testing.T) {
	actions := &mockActions{
		canEnterLowPower: true,
		targetHibernate:  true,
	}

	def := fsm.NewDefinition(actions, 200*time.Millisecond, 200*time.Millisecond)
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
	if !machine.IsInState(fsm.StateLowPowerImminent) {
		t.Fatalf("Expected StateLowPowerImminent, got %v", machine.CurrentState())
	}

	// hibernate-manual upgrade stays in LowPowerImminent
	machine.Send(librefsm.Event{ID: fsm.EvPowerHibernateManual})
	time.Sleep(10 * time.Millisecond)
	if !machine.IsInState(fsm.StateLowPowerImminent) {
		t.Errorf("Expected StateLowPowerImminent after self-upgrade, got %v", machine.CurrentState())
	}
}

// TestWaitingInhibitorsUpgradeToHibernate: upgrades from WaitingInhibitors
// bounce back to LowPowerImminent to restart the imminent sequence.
func TestWaitingInhibitorsUpgradeToHibernate(t *testing.T) {
	actions := &mockActions{
		canEnterLowPower:      true,
		targetSuspend:         true,
		hasBlockingInhibitors: true,
	}

	def := fsm.NewDefinition(actions, 30*time.Millisecond, 30*time.Millisecond)
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
	time.Sleep(50 * time.Millisecond) // past the imminent timeout
	if !machine.IsInState(fsm.StateWaitingInhibitors) {
		t.Fatalf("Expected StateWaitingInhibitors, got %v", machine.CurrentState())
	}

	machine.Send(librefsm.Event{ID: fsm.EvPowerHibernate})
	time.Sleep(10 * time.Millisecond)
	if !machine.IsInState(fsm.StateLowPowerImminent) {
		t.Errorf("Expected StateLowPowerImminent after hibernate upgrade from waiting, got %v", machine.CurrentState())
	}
}

// TestPreSuspendHibernateDowngradeRejected: a suspend command while in
// LowPowerImminent should not downgrade — stays in hibernate path.
func TestHibernateDowngradeRejected(t *testing.T) {
	actions := &mockActions{
		canEnterLowPower: true,
		targetHibernate:  true,
	}

	def := fsm.NewDefinition(actions, 200*time.Millisecond, 200*time.Millisecond)
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
	if !machine.IsInState(fsm.StateLowPowerImminent) {
		t.Fatalf("Expected StateLowPowerImminent, got %v", machine.CurrentState())
	}

	// Suspend command should not downgrade — mockActions always reports
	// IsPowerCommandHigherPriority=true so this test guards against the
	// missing-transition case, not the priority check itself.
	machine.Send(librefsm.Event{ID: fsm.EvPowerSuspend})
	time.Sleep(10 * time.Millisecond)
	if !machine.IsInState(fsm.StateLowPowerImminent) {
		t.Errorf("Expected StateLowPowerImminent (no suspend transition from hibernate), got %v", machine.CurrentState())
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

// TestHibernateManualBufferedWhenVehicleNotInStandby verifies the lock-hibernate
// race fix: when a hibernate-manual command arrives while CanEnterLowPowerState
// rejects (vehicle still in parked/shutting-down), the FSM stays in Running but
// the fallback transition runs OnPowerCommand to record the target. The natural
// EvVehicleStateChanged path picks it up later when stand-by arrives.
func TestHibernateManualBufferedWhenVehicleNotInStandby(t *testing.T) {
	actions := &mockActions{
		canEnterLowPower: false, // vehicle still parked/shutting-down
		targetHibernate:  false,
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

	machine.Send(librefsm.Event{ID: fsm.EvPowerHibernateManual})
	time.Sleep(10 * time.Millisecond)

	// Guarded transition rejected → stay in Running
	if !machine.IsInState(fsm.StateRunning) {
		t.Errorf("Expected StateRunning (vehicle not in stand-by yet), got %v", machine.CurrentState())
	}
	// Fallback transition fired → OnPowerCommand ran, target recorded
	if actions.onPowerCommandCount != 1 {
		t.Errorf("Expected OnPowerCommand to run once via fallback, got count=%d", actions.onPowerCommandCount)
	}
}

// TestStandbyArrivesAfterStoredHibernate verifies the second half of the
// lock-hibernate fix: once the target is stored and vehicle reaches stand-by,
// the natural EvVehicleStateChanged → StateLowPowerImminent transition fires.
func TestStandbyArrivesAfterStoredHibernate(t *testing.T) {
	actions := &mockActions{
		canEnterLowPower: false, // initially: vehicle still parked/shutting-down
		targetHibernate:  false,
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

	// Step 1: hibernate-manual arrives early — buffered via fallback
	machine.Send(librefsm.Event{ID: fsm.EvPowerHibernateManual})
	time.Sleep(10 * time.Millisecond)
	if !machine.IsInState(fsm.StateRunning) {
		t.Fatalf("Expected StateRunning after early hibernate, got %v", machine.CurrentState())
	}

	// Step 2: vehicle reaches stand-by (simulate by flipping the guard flags
	// the way the real Actions impl would after OnPowerCommand stored target).
	actions.canEnterLowPower = true
	actions.targetHibernate = true

	machine.Send(librefsm.Event{ID: fsm.EvVehicleStateChanged})
	time.Sleep(10 * time.Millisecond)

	if !machine.IsInState(fsm.StateLowPowerImminent) {
		t.Errorf("Expected StateLowPowerImminent after stand-by arrival picks up stored target, got %v", machine.CurrentState())
	}
}

// TestHibernationTimerExpiredBufferedWhenVehicleNotInStandby: same race scenario
// for the auto-hibernation timer. Timer fires while vehicle is parked (not yet
// stand-by), target is recorded via fallback, fires later when stand-by arrives.
func TestHibernationTimerExpiredBufferedWhenVehicleNotInStandby(t *testing.T) {
	actions := &mockActions{
		canEnterLowPower: false,
		targetHibernate:  false,
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

	machine.Send(librefsm.Event{ID: fsm.EvHibernationTimerExpired})
	time.Sleep(10 * time.Millisecond)

	if !machine.IsInState(fsm.StateRunning) {
		t.Errorf("Expected StateRunning, got %v", machine.CurrentState())
	}
	if actions.onPowerCommandCount != 1 {
		t.Errorf("Expected OnPowerCommand via fallback, count=%d", actions.onPowerCommandCount)
	}
}

// TestLastDitchCheckFiresInStandby: trigger condition holds while the vehicle
// is in stand-by — the check event goes straight to LowPowerImminent via
// OnLastDitchTriggered.
func TestLastDitchCheckFiresInStandby(t *testing.T) {
	actions := &mockActions{
		canEnterLowPower:   true,
		lastDitchTriggered: true,
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

	machine.Send(librefsm.Event{ID: fsm.EvLastDitchCheck})
	time.Sleep(10 * time.Millisecond)

	if !machine.IsInState(fsm.StateLowPowerImminent) {
		t.Errorf("Expected StateLowPowerImminent, got %v", machine.CurrentState())
	}
	if actions.onLastDitchTriggeredCount != 1 {
		t.Errorf("Expected OnLastDitchTriggered once, count=%d", actions.onLastDitchTriggeredCount)
	}
}

// TestLastDitchCheckNoOpsOutsideStandby: condition holds but the vehicle is
// not in stand-by. Unlike explicit power commands there is no buffering
// fallback — the event must be a pure no-op, and the check sent on the
// stand-by vehicle-state change fires instead.
func TestLastDitchCheckNoOpsOutsideStandby(t *testing.T) {
	actions := &mockActions{
		canEnterLowPower:   false,
		lastDitchTriggered: true,
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

	machine.Send(librefsm.Event{ID: fsm.EvLastDitchCheck})
	time.Sleep(10 * time.Millisecond)

	if !machine.IsInState(fsm.StateRunning) {
		t.Errorf("Expected StateRunning, got %v", machine.CurrentState())
	}
	if actions.onLastDitchTriggeredCount != 0 || actions.onPowerCommandCount != 0 {
		t.Errorf("Expected no actions (no buffering), triggered=%d power=%d",
			actions.onLastDitchTriggeredCount, actions.onPowerCommandCount)
	}

	// Vehicle reaches stand-by; the service re-sends the check.
	actions.canEnterLowPower = true
	machine.Send(librefsm.Event{ID: fsm.EvLastDitchCheck})
	time.Sleep(10 * time.Millisecond)

	if !machine.IsInState(fsm.StateLowPowerImminent) {
		t.Errorf("Expected StateLowPowerImminent after stand-by check, got %v", machine.CurrentState())
	}
}

// TestLastDitchCheckUpgradesPendingSuspend: a suspend in PreSuspend or
// SuspendImminent is upgraded to hibernate when the condition holds.
func TestLastDitchCheckUpgradesPendingSuspend(t *testing.T) {
	actions := &mockActions{
		canEnterLowPower:   true,
		targetSuspend:      true,
		lastDitchTriggered: true,
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
		t.Fatalf("Expected StatePreSuspend, got %v", machine.CurrentState())
	}

	machine.Send(librefsm.Event{ID: fsm.EvLastDitchCheck})
	time.Sleep(10 * time.Millisecond)

	if !machine.IsInState(fsm.StateLowPowerImminent) {
		t.Errorf("Expected StateLowPowerImminent (suspend upgraded), got %v", machine.CurrentState())
	}
}

// TestLastDitchCheckDoesNotRestartImminentSequence: once the hibernate
// sequence is executing, further check events must not re-enter
// LowPowerImminent (that would restart the imminent timer on every input
// update and stall the poweroff).
func TestLastDitchCheckDoesNotRestartImminentSequence(t *testing.T) {
	actions := &mockActions{
		canEnterLowPower:   true,
		lastDitchTriggered: true,
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

	machine.Send(librefsm.Event{ID: fsm.EvLastDitchCheck})
	time.Sleep(10 * time.Millisecond)
	if !machine.IsInState(fsm.StateLowPowerImminent) {
		t.Fatalf("Expected StateLowPowerImminent, got %v", machine.CurrentState())
	}

	machine.Send(librefsm.Event{ID: fsm.EvLastDitchCheck})
	time.Sleep(10 * time.Millisecond)

	if actions.onLastDitchTriggeredCount != 1 {
		t.Errorf("Expected exactly one trigger action, count=%d", actions.onLastDitchTriggeredCount)
	}
}

// TestLastDitchWakeRoutesToHibernate: waking from Suspended with the
// condition holding routes to LowPowerImminent via OnLastDitchWakeup instead
// of re-entering the suspend loop.
func TestLastDitchWakeRoutesToHibernate(t *testing.T) {
	actions := &mockActions{
		canEnterLowPower: true,
		targetSuspend:    true,
	}

	// Long timer delays; the imminent timeout is sent manually so the test
	// is deterministic.
	def := fsm.NewDefinition(actions, 1*time.Second, 1*time.Second)
	machine, err := def.Build()
	if err != nil {
		t.Fatalf("Failed to build FSM: %v", err)
	}

	ctx := context.Background()
	if err := machine.Start(ctx); err != nil {
		t.Fatalf("Failed to start FSM: %v", err)
	}
	defer machine.Stop()

	// Drive into Suspended: suspend command -> imminent -> (timeout) ->
	// waiting inhibitors -> issuing -> issued.
	machine.Send(librefsm.Event{ID: fsm.EvPowerSuspend})
	time.Sleep(10 * time.Millisecond)
	machine.Send(librefsm.Event{ID: fsm.EvSuspendImminentTimeout})
	time.Sleep(10 * time.Millisecond)
	machine.Send(librefsm.Event{ID: fsm.EvInhibitorsChanged})
	time.Sleep(10 * time.Millisecond)
	machine.Send(librefsm.Event{ID: fsm.EvLowPowerIssued})
	time.Sleep(10 * time.Millisecond)
	if !machine.IsInState(fsm.StateSuspended) {
		t.Fatalf("Expected StateSuspended, got %v", machine.CurrentState())
	}

	// CBB drained during suspend: condition holds on resume.
	actions.lastDitchTriggered = true
	machine.Send(librefsm.Event{ID: fsm.EvWakeup})
	time.Sleep(10 * time.Millisecond)

	if !machine.IsInState(fsm.StateLowPowerImminent) {
		t.Errorf("Expected StateLowPowerImminent on last-ditch wake, got %v", machine.CurrentState())
	}
	if actions.onLastDitchWakeupCount != 1 {
		t.Errorf("Expected OnLastDitchWakeup once, count=%d", actions.onLastDitchWakeupCount)
	}
}

// TestRegularWakeUnaffectedByLastDitchGuard: with the condition NOT holding,
// wake from Suspended follows the normal target routing.
func TestRegularWakeUnaffectedByLastDitchGuard(t *testing.T) {
	actions := &mockActions{
		canEnterLowPower: true,
		targetSuspend:    true,
	}

	// Long timer delays; the imminent timeout is sent manually so the test
	// is deterministic and the post-wake PreSuspend state stays observable.
	def := fsm.NewDefinition(actions, 1*time.Second, 1*time.Second)
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
	machine.Send(librefsm.Event{ID: fsm.EvSuspendImminentTimeout})
	time.Sleep(10 * time.Millisecond)
	machine.Send(librefsm.Event{ID: fsm.EvInhibitorsChanged})
	time.Sleep(10 * time.Millisecond)
	machine.Send(librefsm.Event{ID: fsm.EvLowPowerIssued})
	time.Sleep(10 * time.Millisecond)
	if !machine.IsInState(fsm.StateSuspended) {
		t.Fatalf("Expected StateSuspended, got %v", machine.CurrentState())
	}

	machine.Send(librefsm.Event{ID: fsm.EvWakeup})
	time.Sleep(10 * time.Millisecond)

	if !machine.IsInState(fsm.StatePreSuspend) {
		t.Errorf("Expected StatePreSuspend on regular wake, got %v", machine.CurrentState())
	}
	if actions.onLastDitchWakeupCount != 0 {
		t.Errorf("Expected no OnLastDitchWakeup, count=%d", actions.onLastDitchWakeupCount)
	}
}

// TestModemWaitTimeoutProceeds: after the bounded WaitingInhibitors wait, if
// only the modem (or nothing) still blocks, the FSM proceeds with the
// transition rather than stalling on a modem that never dropped its inhibitor.
func TestModemWaitTimeoutProceeds(t *testing.T) {
	actions := &mockActions{
		canEnterLowPower:        true,
		targetSuspend:           true,
		hasBlockingInhibitors:   true, // don't auto-proceed on EvInhibitorsChanged
		canProceedPastModemWait: true, // only the modem still blocks
	}

	def := fsm.NewDefinition(actions, 1*time.Second, 1*time.Second)
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
	machine.Send(librefsm.Event{ID: fsm.EvSuspendImminentTimeout})
	time.Sleep(10 * time.Millisecond)
	if !machine.IsInState(fsm.StateWaitingInhibitors) {
		t.Fatalf("Expected StateWaitingInhibitors, got %v", machine.CurrentState())
	}

	// Modem never dropped its inhibitor; the bounded wait elapses.
	machine.Send(librefsm.Event{ID: fsm.EvInhibitorWaitTimeout})
	time.Sleep(10 * time.Millisecond)
	if !machine.IsInState(fsm.StateIssuingLowPower) {
		t.Errorf("Expected StateIssuingLowPower after modem-wait timeout, got %v", machine.CurrentState())
	}
}

// TestModemWaitTimeoutHeldByRealInhibitor: the bounded wait does NOT bypass a
// genuine block inhibitor (e.g. an OTA install); the FSM keeps waiting.
func TestModemWaitTimeoutHeldByRealInhibitor(t *testing.T) {
	actions := &mockActions{
		canEnterLowPower:        true,
		targetSuspend:           true,
		hasBlockingInhibitors:   true,
		canProceedPastModemWait: false, // a real block inhibitor remains
	}

	def := fsm.NewDefinition(actions, 1*time.Second, 1*time.Second)
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
	machine.Send(librefsm.Event{ID: fsm.EvSuspendImminentTimeout})
	time.Sleep(10 * time.Millisecond)
	if !machine.IsInState(fsm.StateWaitingInhibitors) {
		t.Fatalf("Expected StateWaitingInhibitors, got %v", machine.CurrentState())
	}

	machine.Send(librefsm.Event{ID: fsm.EvInhibitorWaitTimeout})
	time.Sleep(10 * time.Millisecond)
	if !machine.IsInState(fsm.StateWaitingInhibitors) {
		t.Errorf("Expected to stay in StateWaitingInhibitors with a real block inhibitor, got %v", machine.CurrentState())
	}
}
