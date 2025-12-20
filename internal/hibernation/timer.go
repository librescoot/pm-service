package hibernation

import (
	"context"
	"log"
	"sync"
	"time"
)

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

// NewTimer creates a new hibernation timer
func NewTimer(ctx context.Context, logger *log.Logger, defaultDuration time.Duration, onHibernateTimer func()) *Timer {
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
// Setting to 0 disables the timer, negative values are ignored
func (t *Timer) SetTimerValue(timerValueSeconds int32) {
	if timerValueSeconds < 0 {
		return
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
	defer t.mutex.RUnlock()

	// Double-check that we should still trigger hibernation
	if !t.active {
		return
	}

	t.logger.Printf("Hibernation timer expired, triggering hibernation")

	if t.onHibernateTimer != nil {
		t.onHibernateTimer()
	}
}

// Close stops the hibernation timer
func (t *Timer) Close() {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	t.stopTimer()
	t.active = false
}
