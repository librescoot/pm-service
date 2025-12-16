package fsm

import (
	"time"

	"github.com/librescoot/librefsm"
)

// NewDefinition creates the power management FSM definition.
// The actions parameter provides the implementation for state entry/exit,
// guards, and transition actions.
func NewDefinition(actions Actions, preSuspendDelay, suspendImminentDelay time.Duration) *librefsm.Definition {
	return librefsm.NewDefinition().
		// === Main States ===

		State(StateRunning,
			librefsm.WithOnEnter(actions.EnterRunning),
		).

		// Suspend path (battery-sensitive)
		State(StatePreSuspend,
			librefsm.WithOnEnter(actions.EnterPreSuspend),
			librefsm.WithTimeout(preSuspendDelay, EvPreSuspendTimeout),
		).
		State(StateSuspendImminent,
			librefsm.WithOnEnter(actions.EnterSuspendImminent),
			librefsm.WithTimeout(suspendImminentDelay, EvSuspendImminentTimeout),
		).

		// Hibernate path (no battery concern)
		State(StatePreHibernate,
			librefsm.WithOnEnter(actions.EnterPreHibernate),
			librefsm.WithTimeout(preSuspendDelay, EvPreSuspendTimeout),
		).
		State(StateHibernateImminent,
			librefsm.WithOnEnter(actions.EnterHibernateImminent),
			librefsm.WithTimeout(suspendImminentDelay, EvSuspendImminentTimeout),
		).

		// Shared states
		State(StateWaitingInhibitors,
			librefsm.WithOnEnter(actions.EnterWaitingInhibitors),
		).
		State(StateIssuingLowPower,
			librefsm.WithOnEnter(actions.EnterIssuingLowPower),
			librefsm.WithOnExit(actions.ExitIssuingLowPower),
		).
		State(StateSuspended).

		// === Transitions from Running ===

		// Suspend path - power command
		Transition(StateRunning, EvPowerSuspend, StatePreSuspend,
			librefsm.WithGuard(actions.CanEnterLowPowerState),
		).

		// Hibernate path - power commands
		Transition(StateRunning, EvPowerHibernate, StatePreHibernate,
			librefsm.WithGuard(actions.CanEnterLowPowerState),
		).
		Transition(StateRunning, EvPowerHibernateManual, StatePreHibernate,
			librefsm.WithGuard(actions.CanEnterLowPowerState),
		).
		Transition(StateRunning, EvPowerHibernateTimer, StatePreHibernate,
			librefsm.WithGuard(actions.CanEnterLowPowerState),
		).
		Transition(StateRunning, EvPowerReboot, StatePreHibernate,
			librefsm.WithGuard(actions.CanEnterLowPowerState),
		).

		// Auto-hibernation timer expired
		Transition(StateRunning, EvHibernationTimerExpired, StatePreHibernate,
			librefsm.WithGuard(actions.CanEnterLowPowerState),
		).

		// Vehicle state changes may enable low-power transition
		Transition(StateRunning, EvVehicleStateChanged, StatePreSuspend,
			librefsm.WithGuards(actions.CanEnterLowPowerState, actions.IsTargetSuspend),
		).
		Transition(StateRunning, EvVehicleStateChanged, StatePreHibernate,
			librefsm.WithGuards(actions.CanEnterLowPowerState, actions.IsTargetHibernate),
		).

		// Battery became inactive - only relevant for suspend
		Transition(StateRunning, EvBatteryBecameInactive, StatePreSuspend,
			librefsm.WithGuards(actions.CanEnterLowPowerState, actions.IsTargetSuspend),
		).

		// === Transitions from PreSuspend (suspend path) ===

		Transition(StatePreSuspend, EvPreSuspendTimeout, StateSuspendImminent,
			librefsm.WithGuard(actions.CanEnterLowPowerState),
		).

		// Cancel: target set to run
		Transition(StatePreSuspend, EvPowerRun, StateRunning).

		// Cancel: vehicle left standby/parked
		Transition(StatePreSuspend, EvVehicleStateChanged, StateRunning,
			librefsm.WithGuard(actions.IsVehicleNotInStandbyOrParked),
			librefsm.WithAction(actions.OnVehicleLeftLowPowerState),
		).

		// Cancel: battery became active (suspend only - no guard needed!)
		Transition(StatePreSuspend, EvBatteryBecameActive, StateRunning).

		// === Transitions from SuspendImminent (suspend path) ===

		Transition(StateSuspendImminent, EvSuspendImminentTimeout, StateWaitingInhibitors,
			librefsm.WithAction(actions.OnSuspendImminentTimeout),
		).

		// Cancel: target set to run
		Transition(StateSuspendImminent, EvPowerRun, StateRunning).

		// Cancel: vehicle left standby/parked
		Transition(StateSuspendImminent, EvVehicleStateChanged, StateRunning,
			librefsm.WithGuard(actions.IsVehicleNotInStandbyOrParked),
			librefsm.WithAction(actions.OnVehicleLeftLowPowerState),
		).

		// Cancel: battery became active (suspend only - no guard needed!)
		Transition(StateSuspendImminent, EvBatteryBecameActive, StateRunning).

		// === Transitions from PreHibernate (hibernate path) ===

		Transition(StatePreHibernate, EvPreSuspendTimeout, StateHibernateImminent,
			librefsm.WithGuard(actions.CanEnterLowPowerState),
		).

		// Cancel: target set to run
		Transition(StatePreHibernate, EvPowerRun, StateRunning).

		// Cancel: vehicle left standby/parked
		Transition(StatePreHibernate, EvVehicleStateChanged, StateRunning,
			librefsm.WithGuard(actions.IsVehicleNotInStandbyOrParked),
			librefsm.WithAction(actions.OnVehicleLeftLowPowerState),
		).

		// Note: No battery transitions from hibernate path

		// === Transitions from HibernateImminent (hibernate path) ===

		Transition(StateHibernateImminent, EvSuspendImminentTimeout, StateWaitingInhibitors,
			librefsm.WithAction(actions.OnSuspendImminentTimeout),
		).

		// Cancel: target set to run
		Transition(StateHibernateImminent, EvPowerRun, StateRunning).

		// Cancel: vehicle left standby/parked
		Transition(StateHibernateImminent, EvVehicleStateChanged, StateRunning,
			librefsm.WithGuard(actions.IsVehicleNotInStandbyOrParked),
			librefsm.WithAction(actions.OnVehicleLeftLowPowerState),
		).

		// Note: No battery transitions from hibernate path

		// === Transitions from WaitingInhibitors (shared) ===

		// Proceed when no blocking inhibitors
		Transition(StateWaitingInhibitors, EvInhibitorsChanged, StateIssuingLowPower,
			librefsm.WithGuard(actions.HasNoBlockingInhibitors),
		).

		// Try to disable modem if only modem inhibitors remain
		Transition(StateWaitingInhibitors, EvInhibitorsChanged, StateWaitingInhibitors,
			librefsm.WithGuard(actions.HasOnlyModemInhibitors),
			librefsm.WithAction(actions.OnDisableModem),
		).

		// Cancel: target set to run
		Transition(StateWaitingInhibitors, EvPowerRun, StateRunning).

		// Cancel: vehicle left standby/parked
		Transition(StateWaitingInhibitors, EvVehicleStateChanged, StateRunning,
			librefsm.WithGuard(actions.IsVehicleNotInStandbyOrParked),
			librefsm.WithAction(actions.OnVehicleLeftLowPowerState),
		).

		// === Transitions from IssuingLowPower ===

		Transition(StateIssuingLowPower, EvLowPowerIssued, StateSuspended).

		// Wakeup during issuing (e.g., suspend failed or returned quickly)
		Transition(StateIssuingLowPower, EvWakeup, StateRunning,
			librefsm.WithAction(actions.OnWakeup),
		).

		// === Transitions from Suspended ===

		// Wakeup: route to correct path based on target
		Transition(StateSuspended, EvWakeup, StatePreSuspend,
			librefsm.WithGuards(actions.CanEnterLowPowerState, actions.IsTargetSuspend),
			librefsm.WithAction(actions.OnWakeup),
		).
		Transition(StateSuspended, EvWakeup, StatePreHibernate,
			librefsm.WithGuards(actions.CanEnterLowPowerState, actions.IsTargetHibernate),
			librefsm.WithAction(actions.OnWakeup),
		).

		// Wakeup: go to running if conditions don't allow low power
		Transition(StateSuspended, EvWakeup, StateRunning,
			librefsm.WithAction(actions.OnWakeup),
		).

		// RTC wakeup: route to correct imminent state based on target
		Transition(StateSuspended, EvWakeupRTC, StateSuspendImminent,
			librefsm.WithGuards(actions.CanEnterLowPowerState, actions.IsTargetSuspend),
			librefsm.WithAction(actions.OnWakeup),
		).
		Transition(StateSuspended, EvWakeupRTC, StateHibernateImminent,
			librefsm.WithGuards(actions.CanEnterLowPowerState, actions.IsTargetHibernate),
			librefsm.WithAction(actions.OnWakeup),
		).

		// RTC wakeup: go to running if conditions don't allow
		Transition(StateSuspended, EvWakeupRTC, StateRunning,
			librefsm.WithAction(actions.OnWakeup),
		).

		// Initial state
		Initial(StateRunning)
}
