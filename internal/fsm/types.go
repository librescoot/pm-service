package fsm

import (
	"github.com/librescoot/librefsm"
)

// Power management states
const (
	// Main states
	StateRunning         librefsm.StateID = "running"
	StatePreSuspend      librefsm.StateID = "pre-suspend"
	StateSuspendImminent librefsm.StateID = "suspend-imminent"
	// StateLowPowerImminent is the generic "preparing for a low-power
	// transition" state. It's entered for hibernate, hibernate-for,
	// hibernate-manual, hibernate-timer AND reboot — the dispatch at the
	// end of the prep flow picks the actual operation based on
	// fsmData.TargetPowerState. Naming it "hibernate-*" was historical and
	// misleading when seen in reboot logs.
	StateLowPowerImminent  librefsm.StateID = "low-power-imminent"
	StateWaitingInhibitors librefsm.StateID = "waiting-inhibitors"
	StateIssuingLowPower   librefsm.StateID = "issuing-low-power"
	StateSuspended         librefsm.StateID = "suspended"
)

// Events
const (
	// Power commands (from Redis)
	EvPowerRun             librefsm.EventID = "power-run"
	EvPowerSuspend         librefsm.EventID = "power-suspend"
	EvPowerHibernate       librefsm.EventID = "power-hibernate"
	EvPowerHibernateManual librefsm.EventID = "power-hibernate-manual"
	EvPowerHibernateTimer  librefsm.EventID = "power-hibernate-timer"
	EvPowerHibernateFor    librefsm.EventID = "power-hibernate-for"
	EvPowerReboot          librefsm.EventID = "power-reboot"

	// State change events
	EvVehicleStateChanged   librefsm.EventID = "vehicle-state-changed"
	EvBatteryBecameActive   librefsm.EventID = "battery-became-active"
	EvBatteryBecameInactive librefsm.EventID = "battery-became-inactive"

	// Inhibitor events
	EvInhibitorsChanged librefsm.EventID = "inhibitors-changed"

	// Last-ditch hibernate check: sent by the service whenever a trigger
	// input (battery presence/charge, CBB charge, aux voltage, vehicle
	// state) changes and the condition currently holds. Carries a
	// PowerCommandPayload with TargetHibernate so the priority guard and
	// OnPowerCommand work unchanged. Level-triggered: guards decide at
	// transition time, nothing is latched or buffered.
	EvLastDitchCheck librefsm.EventID = "last-ditch-check"

	// Timer events
	EvPreSuspendTimeout       librefsm.EventID = "pre-suspend-timeout"
	EvSuspendImminentTimeout  librefsm.EventID = "suspend-imminent-timeout"
	EvHibernationTimerExpired librefsm.EventID = "hibernation-timer-expired"
	EvDelayInhibitorRemove    librefsm.EventID = "delay-inhibitor-remove"

	// Wakeup and lifecycle
	EvWakeup         librefsm.EventID = "wakeup"
	EvWakeupRTC      librefsm.EventID = "wakeup-rtc" // RTC wakeup skips pre-suspend
	EvLowPowerIssued librefsm.EventID = "low-power-issued"
)

// Timer names
const (
	TimerPreSuspend      = "pre-suspend"
	TimerSuspendImminent = "suspend-imminent"
	TimerHibernationAuto = "hibernation-auto"
	TimerDelayInhibitor  = "delay-inhibitor"
)

// Target power state values (stored in FSMData)
const (
	TargetRun             = "run"
	TargetSuspend         = "suspend"
	TargetHibernate       = "hibernate"
	TargetHibernateManual = "hibernate-manual"
	TargetHibernateTimer  = "hibernate-timer"
	TargetHibernateFor    = "hibernate-for"
	TargetReboot          = "reboot"
)

// Event payload types

type VehicleStatePayload struct {
	State string
}

type BatteryStatePayload struct {
	Slot  string // "0" or "1"
	State string
}

type WakeupPayload struct {
	Reason string
}

type PowerCommandPayload struct {
	TargetState string
	// WakeSeconds is non-zero only for the hibernate-for target: it carries the
	// number of seconds from "now" at which the nRF52 should wake the iMX6.
	WakeSeconds uint32
}

// FSMData holds runtime data for the FSM context
type FSMData struct {
	TargetPowerState    string // run, suspend, hibernate, hibernate-manual, hibernate-timer, hibernate-for, reboot
	VehicleState        string
	BatteryState        string // derived: "active" if either slot is active
	Battery0State       string
	Battery1State       string
	LowPowerStateIssued bool
	ModemDisabled       bool
	WakeupReason        string
	// HibernateForWakeSeconds is the deferred wake-up duration that should be
	// armed on the nRF52 when EnterLowPowerImminent runs. Set by OnPowerCommand
	// from the EvPowerHibernateFor payload and cleared on return to Running.
	HibernateForWakeSeconds uint32
}

// Actions defines the callbacks for the pm-service FSM.
// The Service struct implements this interface.
type Actions interface {
	// State entry actions
	EnterPreSuspend(c *librefsm.Context) error
	EnterSuspendImminent(c *librefsm.Context) error
	EnterLowPowerImminent(c *librefsm.Context) error
	EnterWaitingInhibitors(c *librefsm.Context) error
	EnterIssuingLowPower(c *librefsm.Context) error
	ExitIssuingLowPower(c *librefsm.Context) error

	// Guards
	CanEnterLowPowerState(c *librefsm.Context) bool
	HasNoBlockingInhibitors(c *librefsm.Context) bool
	HasOnlyModemInhibitors(c *librefsm.Context) bool
	IsVehicleNotInStandby(c *librefsm.Context) bool
	IsTargetNotRun(c *librefsm.Context) bool
	IsBatteryNotActive(c *librefsm.Context) bool
	IsTargetSuspend(c *librefsm.Context) bool
	IsTargetHibernate(c *librefsm.Context) bool
	IsLastDitchTriggered(c *librefsm.Context) bool
	IsPowerCommandHigherPriority(c *librefsm.Context) bool

	// Transition actions
	OnPreSuspendTimeout(c *librefsm.Context) error
	OnSuspendImminentTimeout(c *librefsm.Context) error
	OnInhibitorsChanged(c *librefsm.Context) error
	OnWakeup(c *librefsm.Context) error
	OnDisableModem(c *librefsm.Context) error
	OnVehicleStateChanged(c *librefsm.Context) error
	OnVehicleLeftLowPowerState(c *librefsm.Context) error
	OnBatteryStateChanged(c *librefsm.Context) error
	OnPowerCommand(c *librefsm.Context) error
	OnLastDitchTriggered(c *librefsm.Context) error
	OnLastDitchWakeup(c *librefsm.Context) error

	// Publishing
	PublishState(state string) error
	PublishWakeupSource(reason string) error
}
