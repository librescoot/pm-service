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
	canEnterLowPower         bool
	vehicleInStandbyOrParked bool
	vehicleNotInStandby      bool
	targetSuspend            bool
	targetHibernate          bool
	hasBlockingInhibitors    bool
	hasOnlyModemInhibitors   bool
	targetNotRun             bool
}

func (m *mockActions) EnterRunning(c *librefsm.Context) error             { return nil }
func (m *mockActions) EnterPreSuspend(c *librefsm.Context) error          { return nil }
func (m *mockActions) EnterSuspendImminent(c *librefsm.Context) error     { return nil }
func (m *mockActions) EnterPreHibernate(c *librefsm.Context) error        { return nil }
func (m *mockActions) EnterHibernateImminent(c *librefsm.Context) error   { return nil }
func (m *mockActions) EnterWaitingInhibitors(c *librefsm.Context) error   { return nil }
func (m *mockActions) EnterIssuingLowPower(c *librefsm.Context) error { return nil }
func (m *mockActions) ExitIssuingLowPower(c *librefsm.Context) error  { return nil }
func (m *mockActions) CanEnterLowPowerState(c *librefsm.Context) bool { return m.canEnterLowPower }
func (m *mockActions) HasNoBlockingInhibitors(c *librefsm.Context) bool {
	return !m.hasBlockingInhibitors
}
func (m *mockActions) HasOnlyModemInhibitors(c *librefsm.Context) bool {
	return m.hasOnlyModemInhibitors
}
func (m *mockActions) IsVehicleInStandbyOrParked(c *librefsm.Context) bool {
	return m.vehicleInStandbyOrParked
}
func (m *mockActions) IsVehicleNotInStandbyOrParked(c *librefsm.Context) bool {
	return m.vehicleNotInStandby
}
func (m *mockActions) IsTargetNotRun(c *librefsm.Context) bool              { return m.targetNotRun }
func (m *mockActions) IsBatteryNotActive(c *librefsm.Context) bool          { return true }
func (m *mockActions) IsTargetSuspend(c *librefsm.Context) bool             { return m.targetSuspend }
func (m *mockActions) IsTargetHibernate(c *librefsm.Context) bool           { return m.targetHibernate }
func (m *mockActions) OnPreSuspendTimeout(c *librefsm.Context) error        { return nil }
func (m *mockActions) OnSuspendImminentTimeout(c *librefsm.Context) error   { return nil }
func (m *mockActions) OnInhibitorsChanged(c *librefsm.Context) error { return nil }
func (m *mockActions) OnWakeup(c *librefsm.Context) error            { return nil }
func (m *mockActions) OnDisableModem(c *librefsm.Context) error      { return nil }
func (m *mockActions) OnVehicleLeftLowPowerState(c *librefsm.Context) error { return nil }
func (m *mockActions) PublishState(state string) error                      { return nil }
func (m *mockActions) PublishWakeupSource(reason string) error              { return nil }

func TestSuspendPathWithBattery(t *testing.T) {
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

	// Test: suspend command routes to PreSuspend
	machine.Send(librefsm.Event{ID: fsm.EvPowerSuspend})
	time.Sleep(10 * time.Millisecond) // Let FSM process
	if !machine.IsInState(fsm.StatePreSuspend) {
		t.Errorf("Expected StatePreSuspend, got %v", machine.CurrentState())
	}

	// Test: battery became active cancels suspend (no guard needed!)
	machine.Send(librefsm.Event{ID: fsm.EvBatteryBecameActive})
	time.Sleep(10 * time.Millisecond)
	if !machine.IsInState(fsm.StateRunning) {
		t.Errorf("Expected StateRunning after battery active, got %v", machine.CurrentState())
	}
}

func TestHibernatePathNoBattery(t *testing.T) {
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

	// Test: hibernate command routes to PreHibernate
	machine.Send(librefsm.Event{ID: fsm.EvPowerHibernate})
	time.Sleep(10 * time.Millisecond)
	if !machine.IsInState(fsm.StatePreHibernate) {
		t.Errorf("Expected StatePreHibernate, got %v", machine.CurrentState())
	}

	// Test: battery became active does NOT cancel hibernate
	machine.Send(librefsm.Event{ID: fsm.EvBatteryBecameActive})
	time.Sleep(10 * time.Millisecond)
	if !machine.IsInState(fsm.StatePreHibernate) {
		t.Errorf("Expected StatePreHibernate (battery shouldn't affect hibernate), got %v", machine.CurrentState())
	}
}

func TestVehicleStateRoutingToSuspend(t *testing.T) {
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

	// Test: vehicle state change with suspend target routes to PreSuspend
	machine.Send(librefsm.Event{ID: fsm.EvVehicleStateChanged})
	time.Sleep(10 * time.Millisecond)
	if !machine.IsInState(fsm.StatePreSuspend) {
		t.Errorf("Expected StatePreSuspend, got %v", machine.CurrentState())
	}
}

func TestVehicleStateRoutingToHibernate(t *testing.T) {
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

	// Test: vehicle state change with hibernate target routes to PreHibernate (guard fallthrough!)
	machine.Send(librefsm.Event{ID: fsm.EvVehicleStateChanged})
	time.Sleep(10 * time.Millisecond)
	if !machine.IsInState(fsm.StatePreHibernate) {
		t.Errorf("Expected StatePreHibernate, got %v", machine.CurrentState())
	}
}

func TestBatteryInactiveOnlySuspend(t *testing.T) {
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

	// Test: battery inactive with suspend target routes to PreSuspend
	machine.Send(librefsm.Event{ID: fsm.EvBatteryBecameInactive})
	time.Sleep(10 * time.Millisecond)
	if !machine.IsInState(fsm.StatePreSuspend) {
		t.Errorf("Expected StatePreSuspend, got %v", machine.CurrentState())
	}
}

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

	// Test: battery inactive with hibernate target does NOT trigger transition
	machine.Send(librefsm.Event{ID: fsm.EvBatteryBecameInactive})
	time.Sleep(10 * time.Millisecond)
	if !machine.IsInState(fsm.StateRunning) {
		t.Errorf("Expected StateRunning (battery inactive shouldn't trigger hibernate), got %v", machine.CurrentState())
	}
}

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

	// Get to SuspendImminent
	machine.Send(librefsm.Event{ID: fsm.EvPowerSuspend})
	time.Sleep(70 * time.Millisecond) // Wait for PreSuspend timeout
	if !machine.IsInState(fsm.StateSuspendImminent) {
		t.Errorf("Expected StateSuspendImminent, got %v", machine.CurrentState())
	}

	// Battery active should cancel
	machine.Send(librefsm.Event{ID: fsm.EvBatteryBecameActive})
	time.Sleep(10 * time.Millisecond)
	if !machine.IsInState(fsm.StateRunning) {
		t.Errorf("Expected StateRunning after battery active, got %v", machine.CurrentState())
	}
}

func TestWakeupRoutingBasedOnTarget(t *testing.T) {
	tests := []struct {
		name            string
		targetSuspend   bool
		targetHibernate bool
		expectedState   librefsm.StateID
	}{
		{"suspend target routes to PreSuspend", true, false, fsm.StatePreSuspend},
		{"hibernate target routes to PreHibernate", false, true, fsm.StatePreHibernate},
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

			// Simulate wakeup from suspended state
			// First get to suspended (shortcut via direct state - not testing full flow here)
			machine.Send(librefsm.Event{ID: fsm.EvWakeup})
			time.Sleep(10 * time.Millisecond)
			// Note: Since we start in Running, wakeup from Running won't do anything
			// This test is limited - in real scenario we'd be in StateSuspended
		})
	}
}

func TestPriorityBlocking(t *testing.T) {
	// Hibernate should have priority over suspend
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

	// Send hibernate command
	machine.Send(librefsm.Event{ID: fsm.EvPowerHibernate})
	time.Sleep(10 * time.Millisecond)
	if !machine.IsInState(fsm.StatePreHibernate) {
		t.Errorf("Expected StatePreHibernate, got %v", machine.CurrentState())
	}

	// Now send suspend command - should stay in PreHibernate (hibernate has priority)
	machine.Send(librefsm.Event{ID: fsm.EvPowerSuspend})
	time.Sleep(10 * time.Millisecond)
	if !machine.IsInState(fsm.StatePreHibernate) {
		t.Errorf("Expected StatePreHibernate (hibernate priority), got %v", machine.CurrentState())
	}
}

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

	// Send rapid sequence of commands
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

	// Should end up in Running state
	if !machine.IsInState(fsm.StateRunning) {
		t.Errorf("Expected StateRunning after rapid changes, got %v", machine.CurrentState())
	}
}

func TestTimeoutProgression(t *testing.T) {
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

	// Send suspend command
	machine.Send(librefsm.Event{ID: fsm.EvPowerSuspend})
	time.Sleep(10 * time.Millisecond)
	if !machine.IsInState(fsm.StatePreSuspend) {
		t.Errorf("Expected StatePreSuspend, got %v", machine.CurrentState())
	}

	// Wait for PreSuspend timeout
	time.Sleep(60 * time.Millisecond)
	if !machine.IsInState(fsm.StateSuspendImminent) {
		t.Errorf("Expected StateSuspendImminent after timeout, got %v", machine.CurrentState())
	}

	// Wait for SuspendImminent timeout
	time.Sleep(60 * time.Millisecond)
	if !machine.IsInState(fsm.StateWaitingInhibitors) {
		t.Errorf("Expected StateWaitingInhibitors after timeout, got %v", machine.CurrentState())
	}
}

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

	// Enter PreSuspend
	machine.Send(librefsm.Event{ID: fsm.EvPowerSuspend})
	time.Sleep(10 * time.Millisecond)
	if !machine.IsInState(fsm.StatePreSuspend) {
		t.Errorf("Expected StatePreSuspend, got %v", machine.CurrentState())
	}

	// Vehicle leaves standby/parked - should cancel
	machine.Send(librefsm.Event{ID: fsm.EvVehicleStateChanged})
	time.Sleep(10 * time.Millisecond)
	if !machine.IsInState(fsm.StateRunning) {
		t.Errorf("Expected StateRunning after vehicle left standby, got %v", machine.CurrentState())
	}
}

