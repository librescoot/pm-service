package fsm

import (
	"time"

	"github.com/librescoot/librefsm"
)

// NewDefinition creates the power management FSM definition.
// The actions parameter provides the implementation for state entry/exit,
// guards, and transition actions.
//
// Only the natural suspend path (vehicle entering stand-by or battery becoming
// inactive while in stand-by with target=suspend) uses the pre-suspend delay.
// Explicit commands and the hibernate path go directly to the imminent state.
func NewDefinition(actions Actions, preSuspendDelay, suspendImminentDelay time.Duration) *librefsm.Definition {
	return librefsm.NewDefinition().
		// === Main States ===

		State(StateRunning).

		// Suspend path (battery-sensitive) — pre-delay only on natural entry
		State(StatePreSuspend,
			librefsm.WithOnEnter(actions.EnterPreSuspend),
			librefsm.WithTimeout(preSuspendDelay, EvPreSuspendTimeout),
		).
		State(StateSuspendImminent,
			librefsm.WithOnEnter(actions.EnterSuspendImminent),
			librefsm.WithTimeout(suspendImminentDelay, EvSuspendImminentTimeout),
		).

		// Hibernate path (no pre-delay)
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

		// Explicit power commands skip pre-delay
		Transition(StateRunning, EvPowerSuspend, StateSuspendImminent,
			librefsm.WithGuards(actions.IsPowerCommandHigherPriority, actions.CanEnterLowPowerState),
			librefsm.WithAction(actions.OnPowerCommand),
		).
		Transition(StateRunning, EvPowerHibernate, StateHibernateImminent,
			librefsm.WithGuards(actions.IsPowerCommandHigherPriority, actions.CanEnterLowPowerState),
			librefsm.WithAction(actions.OnPowerCommand),
		).
		Transition(StateRunning, EvPowerHibernateManual, StateHibernateImminent,
			librefsm.WithGuards(actions.IsPowerCommandHigherPriority, actions.CanEnterLowPowerState),
			librefsm.WithAction(actions.OnPowerCommand),
		).
		Transition(StateRunning, EvPowerHibernateTimer, StateHibernateImminent,
			librefsm.WithGuards(actions.IsPowerCommandHigherPriority, actions.CanEnterLowPowerState),
			librefsm.WithAction(actions.OnPowerCommand),
		).
		Transition(StateRunning, EvPowerReboot, StateHibernateImminent,
			librefsm.WithGuards(actions.IsPowerCommandHigherPriority, actions.CanEnterLowPowerState),
			librefsm.WithAction(actions.OnPowerCommand),
		).

		// Auto-hibernation timer expired — treated like a command, skip pre-delay
		Transition(StateRunning, EvHibernationTimerExpired, StateHibernateImminent,
			librefsm.WithGuards(actions.IsPowerCommandHigherPriority, actions.CanEnterLowPowerState),
			librefsm.WithAction(actions.OnPowerCommand),
		).

		// Natural suspend path: vehicle enters stand-by with target=suspend — use pre-delay
		Transition(StateRunning, EvVehicleStateChanged, StatePreSuspend,
			librefsm.WithGuards(actions.CanEnterLowPowerState, actions.IsTargetSuspend),
			librefsm.WithAction(actions.OnVehicleStateChanged),
		).
		// Natural hibernate path: vehicle enters stand-by with hibernate target — no pre-delay
		Transition(StateRunning, EvVehicleStateChanged, StateHibernateImminent,
			librefsm.WithGuards(actions.CanEnterLowPowerState, actions.IsTargetHibernate),
			librefsm.WithAction(actions.OnVehicleStateChanged),
		).
		// Catch-all: vehicle state changed but no low-power transition applies
		Transition(StateRunning, EvVehicleStateChanged, StateRunning,
			librefsm.WithAction(actions.OnVehicleStateChanged),
		).

		// Natural suspend path: battery became inactive with target=suspend — use pre-delay
		Transition(StateRunning, EvBatteryBecameInactive, StatePreSuspend,
			librefsm.WithGuards(actions.CanEnterLowPowerState, actions.IsTargetSuspend),
			librefsm.WithAction(actions.OnBatteryStateChanged),
		).
		// Battery state change with no transition
		Transition(StateRunning, EvBatteryBecameInactive, StateRunning,
			librefsm.WithAction(actions.OnBatteryStateChanged),
		).

		// === Transitions from PreSuspend (suspend path) ===

		Transition(StatePreSuspend, EvPreSuspendTimeout, StateSuspendImminent,
			librefsm.WithGuard(actions.CanEnterLowPowerState),
		).

		// Cancel: target set to run
		Transition(StatePreSuspend, EvPowerRun, StateRunning,
			librefsm.WithAction(actions.OnPowerCommand),
		).

		// Cancel: vehicle left stand-by (any other state, including parked)
		Transition(StatePreSuspend, EvVehicleStateChanged, StateRunning,
			librefsm.WithGuard(actions.IsVehicleNotInStandby),
			librefsm.WithAction(actions.OnVehicleLeftLowPowerState),
		).

		// Cancel: battery became active (suspend only - no guard needed!)
		Transition(StatePreSuspend, EvBatteryBecameActive, StateRunning,
			librefsm.WithAction(actions.OnBatteryStateChanged),
		).

		// === Transitions from SuspendImminent (suspend path) ===

		Transition(StateSuspendImminent, EvSuspendImminentTimeout, StateWaitingInhibitors,
			librefsm.WithAction(actions.OnSuspendImminentTimeout),
		).

		// Cancel: target set to run
		Transition(StateSuspendImminent, EvPowerRun, StateRunning,
			librefsm.WithAction(actions.OnPowerCommand),
		).

		// Cancel: vehicle left stand-by
		Transition(StateSuspendImminent, EvVehicleStateChanged, StateRunning,
			librefsm.WithGuard(actions.IsVehicleNotInStandby),
			librefsm.WithAction(actions.OnVehicleLeftLowPowerState),
		).

		// Cancel: battery became active (suspend only - no guard needed!)
		Transition(StateSuspendImminent, EvBatteryBecameActive, StateRunning,
			librefsm.WithAction(actions.OnBatteryStateChanged),
		).

		// === Transitions from HibernateImminent (hibernate path) ===

		Transition(StateHibernateImminent, EvSuspendImminentTimeout, StateWaitingInhibitors,
			librefsm.WithAction(actions.OnSuspendImminentTimeout),
		).

		// Cancel: target set to run
		Transition(StateHibernateImminent, EvPowerRun, StateRunning,
			librefsm.WithAction(actions.OnPowerCommand),
		).

		// Cancel: vehicle left stand-by
		Transition(StateHibernateImminent, EvVehicleStateChanged, StateRunning,
			librefsm.WithGuard(actions.IsVehicleNotInStandby),
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
		Transition(StateWaitingInhibitors, EvPowerRun, StateRunning,
			librefsm.WithAction(actions.OnPowerCommand),
		).

		// Cancel: vehicle left stand-by
		Transition(StateWaitingInhibitors, EvVehicleStateChanged, StateRunning,
			librefsm.WithGuard(actions.IsVehicleNotInStandby),
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
		Transition(StateSuspended, EvWakeup, StateHibernateImminent,
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

		// Keep battery state current while suspended so wakeup guards see the right value
		Transition(StateSuspended, EvBatteryBecameActive, StateSuspended,
			librefsm.WithAction(actions.OnBatteryStateChanged),
		).
		Transition(StateSuspended, EvBatteryBecameInactive, StateSuspended,
			librefsm.WithAction(actions.OnBatteryStateChanged),
		).

		// Initial state
		Initial(StateRunning)
}
