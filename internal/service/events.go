package service

// EventType represents the type of event in the system
type EventType int

const (
	EventPowerCommand EventType = iota
	EventGovernorCommand
	EventVehicleState
	EventBatteryState
	EventInhibitorsChanged
	EventPreSuspendElapsed
	EventSuspendImminentElapsed
	EventLowPowerStateEnter
	EventLowPowerStateExit
	EventWakeup
	EventHibernationTimerExpired
	EventHibernationComplete
	EventHibernationSettingsChanged
	EventDelayInhibitorRemove
)

// Event represents an event in the system
type Event struct {
	Type EventType
	Data interface{}
}

// PowerCommandData contains data for power command events
type PowerCommandData struct {
	Command string
}

// GovernorCommandData contains data for governor command events
type GovernorCommandData struct {
	Governor string
}

// VehicleStateData contains data for vehicle state events
type VehicleStateData struct {
	State string
}

// BatteryStateData contains data for battery state events
type BatteryStateData struct {
	State string
}

// LowPowerStateEnterData contains data for low power state enter events
type LowPowerStateEnterData struct {
	State string
}

// WakeupData contains data for wakeup events
type WakeupData struct {
	Reason string
}

// HibernationSettingsData contains data for hibernation settings events
type HibernationSettingsData struct {
	TimerValue int32
}
