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
		State(StateLowPowerImminent,
			librefsm.WithOnEnter(actions.EnterLowPowerImminent),
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

		// Explicit power commands skip pre-delay.
		//
		// Each command has two transitions: a guarded one that fires when the
		// vehicle is already in stand-by, and a self-transition fallback that
		// records the target via OnPowerCommand when CanEnterLowPowerState
		// rejects (e.g., vehicle still in parked/shutting-down). The natural
		// EvVehicleStateChanged transitions below pick up the stored target
		// when stand-by arrives, mirroring the OEM unu-pm setTargetMode +
		// onVehicleState pattern. Without the fallback, lock-hibernate races
		// with vehicle.state updates and gets dropped.
		Transition(StateRunning, EvPowerSuspend, StateSuspendImminent,
			librefsm.WithGuards(actions.IsPowerCommandHigherPriority, actions.CanEnterLowPowerState),
			librefsm.WithAction(actions.OnPowerCommand),
		).
		Transition(StateRunning, EvPowerSuspend, StateRunning,
			librefsm.WithGuard(actions.IsPowerCommandHigherPriority),
			librefsm.WithAction(actions.OnPowerCommand),
		).
		Transition(StateRunning, EvPowerHibernate, StateLowPowerImminent,
			librefsm.WithGuards(actions.IsPowerCommandHigherPriority, actions.CanEnterLowPowerState),
			librefsm.WithAction(actions.OnPowerCommand),
		).
		Transition(StateRunning, EvPowerHibernate, StateRunning,
			librefsm.WithGuard(actions.IsPowerCommandHigherPriority),
			librefsm.WithAction(actions.OnPowerCommand),
		).
		Transition(StateRunning, EvPowerHibernateManual, StateLowPowerImminent,
			librefsm.WithGuards(actions.IsPowerCommandHigherPriority, actions.CanEnterLowPowerState),
			librefsm.WithAction(actions.OnPowerCommand),
		).
		Transition(StateRunning, EvPowerHibernateManual, StateRunning,
			librefsm.WithGuard(actions.IsPowerCommandHigherPriority),
			librefsm.WithAction(actions.OnPowerCommand),
		).
		Transition(StateRunning, EvPowerHibernateFor, StateLowPowerImminent,
			librefsm.WithGuards(actions.IsPowerCommandHigherPriority, actions.CanEnterLowPowerState),
			librefsm.WithAction(actions.OnPowerCommand),
		).
		Transition(StateRunning, EvPowerHibernateFor, StateRunning,
			librefsm.WithGuard(actions.IsPowerCommandHigherPriority),
			librefsm.WithAction(actions.OnPowerCommand),
		).
		Transition(StateRunning, EvPowerHibernateTimer, StateLowPowerImminent,
			librefsm.WithGuards(actions.IsPowerCommandHigherPriority, actions.CanEnterLowPowerState),
			librefsm.WithAction(actions.OnPowerCommand),
		).
		Transition(StateRunning, EvPowerHibernateTimer, StateRunning,
			librefsm.WithGuard(actions.IsPowerCommandHigherPriority),
			librefsm.WithAction(actions.OnPowerCommand),
		).
		Transition(StateRunning, EvPowerReboot, StateLowPowerImminent,
			librefsm.WithGuards(actions.IsPowerCommandHigherPriority, actions.CanEnterLowPowerState),
			librefsm.WithAction(actions.OnPowerCommand),
		).
		Transition(StateRunning, EvPowerReboot, StateRunning,
			librefsm.WithGuard(actions.IsPowerCommandHigherPriority),
			librefsm.WithAction(actions.OnPowerCommand),
		).

		// Auto-hibernation timer expired — treated like a command, skip pre-delay
		Transition(StateRunning, EvHibernationTimerExpired, StateLowPowerImminent,
			librefsm.WithGuards(actions.IsPowerCommandHigherPriority, actions.CanEnterLowPowerState),
			librefsm.WithAction(actions.OnPowerCommand),
		).
		Transition(StateRunning, EvHibernationTimerExpired, StateRunning,
			librefsm.WithGuard(actions.IsPowerCommandHigherPriority),
			librefsm.WithAction(actions.OnPowerCommand),
		).

		// EvPowerRun in Running clears any buffered low-power target.
		// Counterpart to the fallback transitions above.
		Transition(StateRunning, EvPowerRun, StateRunning,
			librefsm.WithAction(actions.OnPowerCommand),
		).

		// Natural suspend path: vehicle enters stand-by with target=suspend — use pre-delay
		Transition(StateRunning, EvVehicleStateChanged, StatePreSuspend,
			librefsm.WithGuards(actions.CanEnterLowPowerState, actions.IsTargetSuspend),
			librefsm.WithAction(actions.OnVehicleStateChanged),
		).
		// Natural hibernate path: vehicle enters stand-by with hibernate target — no pre-delay
		Transition(StateRunning, EvVehicleStateChanged, StateLowPowerImminent,
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

		// Upgrade: higher-priority hibernate-path events jump straight to LowPowerImminent
		Transition(StatePreSuspend, EvPowerHibernate, StateLowPowerImminent,
			librefsm.WithGuards(actions.IsPowerCommandHigherPriority, actions.CanEnterLowPowerState),
			librefsm.WithAction(actions.OnPowerCommand),
		).
		Transition(StatePreSuspend, EvPowerHibernateManual, StateLowPowerImminent,
			librefsm.WithGuards(actions.IsPowerCommandHigherPriority, actions.CanEnterLowPowerState),
			librefsm.WithAction(actions.OnPowerCommand),
		).
		Transition(StatePreSuspend, EvPowerHibernateFor, StateLowPowerImminent,
			librefsm.WithGuards(actions.IsPowerCommandHigherPriority, actions.CanEnterLowPowerState),
			librefsm.WithAction(actions.OnPowerCommand),
		).
		Transition(StatePreSuspend, EvPowerHibernateTimer, StateLowPowerImminent,
			librefsm.WithGuards(actions.IsPowerCommandHigherPriority, actions.CanEnterLowPowerState),
			librefsm.WithAction(actions.OnPowerCommand),
		).
		Transition(StatePreSuspend, EvPowerReboot, StateLowPowerImminent,
			librefsm.WithGuards(actions.IsPowerCommandHigherPriority, actions.CanEnterLowPowerState),
			librefsm.WithAction(actions.OnPowerCommand),
		).
		Transition(StatePreSuspend, EvHibernationTimerExpired, StateLowPowerImminent,
			librefsm.WithGuards(actions.IsPowerCommandHigherPriority, actions.CanEnterLowPowerState),
			librefsm.WithAction(actions.OnPowerCommand),
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

		// Upgrade: higher-priority hibernate-path events jump to LowPowerImminent
		// (re-enters imminent state, restarting the imminent timer)
		Transition(StateSuspendImminent, EvPowerHibernate, StateLowPowerImminent,
			librefsm.WithGuards(actions.IsPowerCommandHigherPriority, actions.CanEnterLowPowerState),
			librefsm.WithAction(actions.OnPowerCommand),
		).
		Transition(StateSuspendImminent, EvPowerHibernateManual, StateLowPowerImminent,
			librefsm.WithGuards(actions.IsPowerCommandHigherPriority, actions.CanEnterLowPowerState),
			librefsm.WithAction(actions.OnPowerCommand),
		).
		Transition(StateSuspendImminent, EvPowerHibernateFor, StateLowPowerImminent,
			librefsm.WithGuards(actions.IsPowerCommandHigherPriority, actions.CanEnterLowPowerState),
			librefsm.WithAction(actions.OnPowerCommand),
		).
		Transition(StateSuspendImminent, EvPowerHibernateTimer, StateLowPowerImminent,
			librefsm.WithGuards(actions.IsPowerCommandHigherPriority, actions.CanEnterLowPowerState),
			librefsm.WithAction(actions.OnPowerCommand),
		).
		Transition(StateSuspendImminent, EvPowerReboot, StateLowPowerImminent,
			librefsm.WithGuards(actions.IsPowerCommandHigherPriority, actions.CanEnterLowPowerState),
			librefsm.WithAction(actions.OnPowerCommand),
		).
		Transition(StateSuspendImminent, EvHibernationTimerExpired, StateLowPowerImminent,
			librefsm.WithGuards(actions.IsPowerCommandHigherPriority, actions.CanEnterLowPowerState),
			librefsm.WithAction(actions.OnPowerCommand),
		).

		// === Transitions from LowPowerImminent (hibernate path) ===

		Transition(StateLowPowerImminent, EvSuspendImminentTimeout, StateWaitingInhibitors,
			librefsm.WithAction(actions.OnSuspendImminentTimeout),
		).

		// Cancel: target set to run
		Transition(StateLowPowerImminent, EvPowerRun, StateRunning,
			librefsm.WithAction(actions.OnPowerCommand),
		).

		// Cancel: vehicle left stand-by
		Transition(StateLowPowerImminent, EvVehicleStateChanged, StateRunning,
			librefsm.WithGuard(actions.IsVehicleNotInStandby),
			librefsm.WithAction(actions.OnVehicleLeftLowPowerState),
		).

		// Upgrade within hibernate priorities (e.g., timer → hibernate → hibernate-manual).
		// Self-transitions re-enter the state and restart the imminent timer.
		Transition(StateLowPowerImminent, EvPowerHibernate, StateLowPowerImminent,
			librefsm.WithGuards(actions.IsPowerCommandHigherPriority, actions.CanEnterLowPowerState),
			librefsm.WithAction(actions.OnPowerCommand),
		).
		Transition(StateLowPowerImminent, EvPowerHibernateManual, StateLowPowerImminent,
			librefsm.WithGuards(actions.IsPowerCommandHigherPriority, actions.CanEnterLowPowerState),
			librefsm.WithAction(actions.OnPowerCommand),
		).
		Transition(StateLowPowerImminent, EvPowerHibernateFor, StateLowPowerImminent,
			librefsm.WithGuards(actions.IsPowerCommandHigherPriority, actions.CanEnterLowPowerState),
			librefsm.WithAction(actions.OnPowerCommand),
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

		// Cancel: battery became active during waiting (only relevant for suspend target)
		Transition(StateWaitingInhibitors, EvBatteryBecameActive, StateRunning,
			librefsm.WithGuard(actions.IsTargetSuspend),
			librefsm.WithAction(actions.OnBatteryStateChanged),
		).

		// Upgrade: higher-priority events bounce back to LowPowerImminent
		// to restart the imminent sequence under the new target.
		Transition(StateWaitingInhibitors, EvPowerHibernate, StateLowPowerImminent,
			librefsm.WithGuards(actions.IsPowerCommandHigherPriority, actions.CanEnterLowPowerState),
			librefsm.WithAction(actions.OnPowerCommand),
		).
		Transition(StateWaitingInhibitors, EvPowerHibernateManual, StateLowPowerImminent,
			librefsm.WithGuards(actions.IsPowerCommandHigherPriority, actions.CanEnterLowPowerState),
			librefsm.WithAction(actions.OnPowerCommand),
		).
		Transition(StateWaitingInhibitors, EvPowerHibernateFor, StateLowPowerImminent,
			librefsm.WithGuards(actions.IsPowerCommandHigherPriority, actions.CanEnterLowPowerState),
			librefsm.WithAction(actions.OnPowerCommand),
		).
		Transition(StateWaitingInhibitors, EvPowerHibernateTimer, StateLowPowerImminent,
			librefsm.WithGuards(actions.IsPowerCommandHigherPriority, actions.CanEnterLowPowerState),
			librefsm.WithAction(actions.OnPowerCommand),
		).
		Transition(StateWaitingInhibitors, EvPowerReboot, StateLowPowerImminent,
			librefsm.WithGuards(actions.IsPowerCommandHigherPriority, actions.CanEnterLowPowerState),
			librefsm.WithAction(actions.OnPowerCommand),
		).
		Transition(StateWaitingInhibitors, EvHibernationTimerExpired, StateLowPowerImminent,
			librefsm.WithGuards(actions.IsPowerCommandHigherPriority, actions.CanEnterLowPowerState),
			librefsm.WithAction(actions.OnPowerCommand),
		).

		// === Transitions from IssuingLowPower ===

		Transition(StateIssuingLowPower, EvLowPowerIssued, StateSuspended).

		// Wakeup from suspend: route to correct path based on target (same logic as StateSuspended)
		Transition(StateIssuingLowPower, EvWakeup, StatePreSuspend,
			librefsm.WithGuards(actions.CanEnterLowPowerState, actions.IsTargetSuspend),
			librefsm.WithAction(actions.OnWakeup),
		).
		Transition(StateIssuingLowPower, EvWakeup, StateLowPowerImminent,
			librefsm.WithGuards(actions.CanEnterLowPowerState, actions.IsTargetHibernate),
			librefsm.WithAction(actions.OnWakeup),
		).
		Transition(StateIssuingLowPower, EvWakeup, StateRunning,
			librefsm.WithAction(actions.OnWakeup),
		).

		// RTC wakeup from suspend: skip pre-delay (same logic as StateSuspended)
		Transition(StateIssuingLowPower, EvWakeupRTC, StateSuspendImminent,
			librefsm.WithGuards(actions.CanEnterLowPowerState, actions.IsTargetSuspend),
			librefsm.WithAction(actions.OnWakeup),
		).
		Transition(StateIssuingLowPower, EvWakeupRTC, StateLowPowerImminent,
			librefsm.WithGuards(actions.CanEnterLowPowerState, actions.IsTargetHibernate),
			librefsm.WithAction(actions.OnWakeup),
		).
		Transition(StateIssuingLowPower, EvWakeupRTC, StateRunning,
			librefsm.WithAction(actions.OnWakeup),
		).

		// === Transitions from Suspended ===

		// Wakeup: route to correct path based on target
		Transition(StateSuspended, EvWakeup, StatePreSuspend,
			librefsm.WithGuards(actions.CanEnterLowPowerState, actions.IsTargetSuspend),
			librefsm.WithAction(actions.OnWakeup),
		).
		Transition(StateSuspended, EvWakeup, StateLowPowerImminent,
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
		Transition(StateSuspended, EvWakeupRTC, StateLowPowerImminent,
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
