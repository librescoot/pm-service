package fsm

import (
	"github.com/librescoot/librefsm"
)

// Power management states
const (
	// Main states
	StateRunning           librefsm.StateID = "running"
	StatePreSuspend        librefsm.StateID = "pre-suspend"
	StateSuspendImminent   librefsm.StateID = "suspend-imminent"
	StatePreHibernate      librefsm.StateID = "pre-hibernate"
	StateHibernateImminent librefsm.StateID = "hibernate-imminent"
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
	EvPowerReboot          librefsm.EventID = "power-reboot"

	// State change events
	EvVehicleStateChanged   librefsm.EventID = "vehicle-state-changed"
	EvBatteryBecameActive   librefsm.EventID = "battery-became-active"
	EvBatteryBecameInactive librefsm.EventID = "battery-became-inactive"

	// Inhibitor events
	EvInhibitorsChanged librefsm.EventID = "inhibitors-changed"

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
	TargetReboot          = "reboot"
)

// Event payload types

type VehicleStatePayload struct {
	State string
}

type BatteryStatePayload struct {
	State string
}

type WakeupPayload struct {
	Reason string
}

type PowerCommandPayload struct {
	TargetState string
}

// FSMData holds runtime data for the FSM context
type FSMData struct {
	TargetPowerState    string // run, suspend, hibernate, hibernate-manual, hibernate-timer, reboot
	VehicleState        string
	BatteryState        string
	LowPowerStateIssued bool
	ModemDisabled       bool
	WakeupReason        string
}

// Actions defines the callbacks for the pm-service FSM.
// The Service struct implements this interface.
type Actions interface {
	// State entry actions
	EnterRunning(c *librefsm.Context) error
	EnterPreSuspend(c *librefsm.Context) error
	EnterSuspendImminent(c *librefsm.Context) error
	EnterPreHibernate(c *librefsm.Context) error
	EnterHibernateImminent(c *librefsm.Context) error
	EnterWaitingInhibitors(c *librefsm.Context) error
	EnterIssuingLowPower(c *librefsm.Context) error
	ExitIssuingLowPower(c *librefsm.Context) error

	// Guards
	CanEnterLowPowerState(c *librefsm.Context) bool
	HasNoBlockingInhibitors(c *librefsm.Context) bool
	HasOnlyModemInhibitors(c *librefsm.Context) bool
	IsVehicleInStandbyOrParked(c *librefsm.Context) bool
	IsVehicleNotInStandbyOrParked(c *librefsm.Context) bool
	IsTargetNotRun(c *librefsm.Context) bool
	IsBatteryNotActive(c *librefsm.Context) bool
	IsTargetSuspend(c *librefsm.Context) bool
	IsTargetHibernate(c *librefsm.Context) bool

	// Transition actions
	OnPreSuspendTimeout(c *librefsm.Context) error
	OnSuspendImminentTimeout(c *librefsm.Context) error
	OnInhibitorsChanged(c *librefsm.Context) error
	OnWakeup(c *librefsm.Context) error
	OnDisableModem(c *librefsm.Context) error
	OnVehicleLeftLowPowerState(c *librefsm.Context) error

	// Publishing
	PublishState(state string) error
	PublishWakeupSource(reason string) error
}
