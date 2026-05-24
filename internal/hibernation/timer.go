package hibernation

import (
	"context"
	"log"
	"sync"
	"time"
)

// MinTimerSeconds is the floor enforced on any non-zero hibernation timer
// value. Values below this would risk hibernating the scooter almost as soon
// as it enters standby, which is virtually never what the user wants.
// Setting the timer to 0 still disables it; anything between 1 and
// MinTimerSeconds-1 is clamped up to MinTimerSeconds.
const MinTimerSeconds = 300

// Timer manages the hibernation timer that triggers hibernation after extended standby
type Timer struct {
	mutex            sync.RWMutex
	logger           *log.Logger
	ctx              context.Context
	timer            *time.Timer
	timerDuration    time.Duration
	lastActivateTime time.Time
	active           bool   // Vehicle is in standby
	enabled          bool   // Timer value > 0
	onHibernateTimer func() // Callback when hibernation timer triggers
}

// NewTimer creates a new hibernation timer. defaultDuration is clamped up to
// MinTimerSeconds when non-zero, matching the runtime floor enforced by
// SetTimerValue.
func NewTimer(ctx context.Context, logger *log.Logger, defaultDuration time.Duration, onHibernateTimer func()) *Timer {
	floor := time.Duration(MinTimerSeconds) * time.Second
	if defaultDuration > 0 && defaultDuration < floor {
		logger.Printf("Hibernation timer default %v below floor; clamping to %v",
			defaultDuration, floor)
		defaultDuration = floor
	}
	return &Timer{
		logger:           logger,
		ctx:              ctx,
		timerDuration:    defaultDuration,
		active:           false,
		enabled:          defaultDuration > 0,
		onHibernateTimer: onHibernateTimer,
	}
}

// SetTimerValue sets the hibernation timer duration in seconds
// Setting to 0 disables the timer, negative values are ignored.
// Non-zero values below MinTimerSeconds are clamped up to MinTimerSeconds
// so a stray 30s setting can't hibernate the scooter the moment it parks.
func (t *Timer) SetTimerValue(timerValueSeconds int32) {
	if timerValueSeconds < 0 {
		return
	}

	if timerValueSeconds > 0 && timerValueSeconds < MinTimerSeconds {
		t.logger.Printf("Hibernation timer value %d below floor; clamping to %d",
			timerValueSeconds, MinTimerSeconds)
		timerValueSeconds = MinTimerSeconds
	}

	t.mutex.Lock()
	defer t.mutex.Unlock()

	t.timerDuration = time.Duration(timerValueSeconds) * time.Second

	if timerValueSeconds == 0 {
		t.enabled = false
		t.stopTimer()
		t.logger.Printf("Hibernation timer disabled by configuration")
	} else {
		wasEnabled := t.enabled
		t.enabled = true

		if t.active {
			if wasEnabled {
				// Timer was already running, update it
				timeSinceLastActivate := time.Since(t.lastActivateTime)
				if t.timerDuration > timeSinceLastActivate {
					updatedTimer := t.timerDuration - timeSinceLastActivate
					t.startTimer(updatedTimer)
					t.logger.Printf("Hibernation timer updated with %d seconds remaining",
						int32(updatedTimer.Seconds()))
				} else {
					// Timer should have already triggered, trigger immediately
					t.logger.Printf("Hibernation timer expired, triggering hibernation immediately")
					go t.onTimer()
				}
			} else {
				// Timer was disabled, now enabled with active vehicle
				t.startTimer(t.timerDuration)
				t.logger.Printf("Hibernation timer started with %d seconds",
					int32(t.timerDuration.Seconds()))
			}
		}
	}
}

// ResetTimer activates/deactivates the hibernation timer based on vehicle state
// activate=true when vehicle enters standby, false when it leaves standby
func (t *Timer) ResetTimer(activate bool) {
	t.mutex.Lock()
	defer t.mutex.Unlock()

	if !t.enabled {
		t.active = activate
		return
	}

	if activate {
		// Vehicle entering standby, (re)start timer
		t.lastActivateTime = time.Now()
		t.startTimer(t.timerDuration)
		t.logger.Printf("Hibernation timer started with %d seconds",
			int32(t.timerDuration.Seconds()))
	} else if t.active {
		// Vehicle leaving standby, stop timer
		t.stopTimer()
		t.logger.Printf("Hibernation timer stopped")
	}

	t.active = activate
}

// GetActive returns whether the hibernation timer is currently active
func (t *Timer) GetActive() bool {
	t.mutex.RLock()
	defer t.mutex.RUnlock()
	return t.active
}

// GetTimerDuration returns the configured timer duration
func (t *Timer) GetTimerDuration() time.Duration {
	t.mutex.RLock()
	defer t.mutex.RUnlock()
	return t.timerDuration
}

// startTimer starts the hibernation timer with the given duration
func (t *Timer) startTimer(duration time.Duration) {
	t.stopTimer()
	t.timer = time.AfterFunc(duration, t.onTimer)
}

// stopTimer stops the hibernation timer
func (t *Timer) stopTimer() {
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
}

// onTimer is called when the hibernation timer expires
func (t *Timer) onTimer() {
	t.mutex.RLock()
	if !t.active {
		t.mutex.RUnlock()
		return
	}
	cb := t.onHibernateTimer
	t.mutex.RUnlock()

	t.logger.Printf("Hibernation timer expired, triggering hibernation")

	if cb != nil {
		cb()
	}
}

// Close stops the hibernation timer
func (t *Timer) Close() {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	t.stopTimer()
	t.active = false
}
