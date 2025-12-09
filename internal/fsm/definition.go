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

		State(StatePreSuspend,
			librefsm.WithOnEnter(actions.EnterPreSuspend),
			librefsm.WithTimeout(preSuspendDelay, EvPreSuspendTimeout),
		).

		State(StateSuspendImminent,
			librefsm.WithOnEnter(actions.EnterSuspendImminent),
			librefsm.WithTimeout(suspendImminentDelay, EvSuspendImminentTimeout),
		).

		State(StateWaitingInhibitors,
			librefsm.WithOnEnter(actions.EnterWaitingInhibitors),
		).

		State(StateIssuingLowPower,
			librefsm.WithOnEnter(actions.EnterIssuingLowPower),
			librefsm.WithOnExit(actions.ExitIssuingLowPower),
		).

		State(StateSuspended).

		// === Hibernation Parent State ===

		State(StateHibernation,
			librefsm.WithOnEnter(actions.EnterHibernation),
			librefsm.WithOnExit(actions.ExitHibernation),
		).

		// === Hibernation Substates ===

		State(StateHibernationWaiting,
			librefsm.WithParent(StateHibernation),
			librefsm.WithOnEnter(actions.EnterHibernationWaiting),
			librefsm.WithTimeout(HibernationStartTime, EvHibernationStartTimeout),
		).

		State(StateHibernationAdvanced,
			librefsm.WithParent(StateHibernation),
			librefsm.WithOnEnter(actions.EnterHibernationAdvanced),
			librefsm.WithTimeout(HibernationAdvanceTime, EvHibernationAdvanceTimeout),
		).

		State(StateHibernationSeatbox,
			librefsm.WithParent(StateHibernation),
			librefsm.WithOnEnter(actions.EnterHibernationSeatbox),
			librefsm.WithTimeout(HibernationExitTime, EvHibernationExitTimeout),
		).

		State(StateHibernationConfirm,
			librefsm.WithParent(StateHibernation),
			librefsm.WithOnEnter(actions.EnterHibernationConfirm),
			librefsm.WithTimeout(HibernationConfirmTime, EvHibernationConfirmTimeout),
		).

		// === Transitions from Running ===

		// Power commands trigger pre-suspend if conditions allow
		Transition(StateRunning, EvPowerSuspend, StatePreSuspend,
			librefsm.WithGuard(actions.CanEnterLowPowerState),
		).
		Transition(StateRunning, EvPowerHibernate, StatePreSuspend,
			librefsm.WithGuard(actions.CanEnterLowPowerState),
		).
		Transition(StateRunning, EvPowerHibernateManual, StatePreSuspend,
			librefsm.WithGuard(actions.CanEnterLowPowerState),
		).
		Transition(StateRunning, EvPowerHibernateTimer, StatePreSuspend,
			librefsm.WithGuard(actions.CanEnterLowPowerState),
		).
		Transition(StateRunning, EvPowerReboot, StatePreSuspend,
			librefsm.WithGuard(actions.CanEnterLowPowerState),
		).

		// Auto-hibernation timer expired
		Transition(StateRunning, EvHibernationTimerExpired, StatePreSuspend,
			librefsm.WithGuard(actions.CanEnterLowPowerState),
		).

		// Manual hibernation sequence start
		Transition(StateRunning, EvHibernationStart, StateHibernationWaiting,
			librefsm.WithGuard(actions.IsVehicleInStandbyOrParked),
		).

		// State changes may enable low-power transition (e.g., vehicle entered standby)
		Transition(StateRunning, EvVehicleStateChanged, StatePreSuspend,
			librefsm.WithGuard(actions.CanEnterLowPowerState),
		).
		Transition(StateRunning, EvBatteryStateChanged, StatePreSuspend,
			librefsm.WithGuard(actions.CanEnterLowPowerState),
		).

		// === Transitions from PreSuspend ===

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

		// === Transitions from SuspendImminent ===

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

		// === Transitions from WaitingInhibitors ===

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

		// Wakeup: go back to pre-suspend if target is still low power
		Transition(StateSuspended, EvWakeup, StatePreSuspend,
			librefsm.WithGuard(actions.IsTargetNotRun),
			librefsm.WithAction(actions.OnWakeup),
		).

		// Wakeup: go to running if target is run
		Transition(StateSuspended, EvWakeup, StateRunning,
			librefsm.WithAction(actions.OnWakeup),
		).

		// === Manual Hibernation Sequence Transitions ===

		// Waiting → Advanced on timeout
		Transition(StateHibernationWaiting, EvHibernationStartTimeout, StateHibernationAdvanced).

		// Waiting → Running on input release or cancel
		Transition(StateHibernationWaiting, EvHibernationInputReleased, StateRunning).
		Transition(StateHibernationWaiting, EvHibernationCancel, StateRunning).

		// Advanced → Seatbox on timeout
		Transition(StateHibernationAdvanced, EvHibernationAdvanceTimeout, StateHibernationSeatbox).

		// Advanced → Running on input release or cancel
		Transition(StateHibernationAdvanced, EvHibernationInputReleased, StateRunning).
		Transition(StateHibernationAdvanced, EvHibernationCancel, StateRunning).

		// Seatbox → Confirm on seatbox closed
		Transition(StateHibernationSeatbox, EvSeatboxClosed, StateHibernationConfirm).

		// Seatbox → Running on timeout, input release, or cancel
		Transition(StateHibernationSeatbox, EvHibernationExitTimeout, StateRunning).
		Transition(StateHibernationSeatbox, EvHibernationInputReleased, StateRunning).
		Transition(StateHibernationSeatbox, EvHibernationCancel, StateRunning).

		// Confirm → PreSuspend on final button press (completes sequence)
		Transition(StateHibernationConfirm, EvHibernationInputPressed, StatePreSuspend,
			librefsm.WithAction(actions.OnHibernationComplete),
		).

		// Confirm → Running on timeout or cancel
		Transition(StateHibernationConfirm, EvHibernationConfirmTimeout, StateRunning).
		Transition(StateHibernationConfirm, EvHibernationCancel, StateRunning).

		// === Global Hibernation Transitions (via parent) ===

		// Cancel hibernation sequence if vehicle state changes
		Transition(StateHibernation, EvVehicleStateChanged, StateRunning,
			librefsm.WithGuard(actions.IsVehicleNotInStandbyOrParked),
		).

		// Cancel on power-run command
		Transition(StateHibernation, EvPowerRun, StateRunning).

		// Initial state
		Initial(StateRunning)
}
