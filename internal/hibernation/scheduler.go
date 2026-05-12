package hibernation

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// Scheduler runs a single cron-style hibernation schedule.
//
// Semantics:
//   - When the cron expression fires, the scheduler treats fire-time + duration
//     as the desired wake-by wall-clock time. If the vehicle is already in
//     standby, the hibernation request is dispatched immediately with that
//     remaining-time value. If the vehicle is in an active state, the request
//     is deferred until the next transition to standby; the wake-by time is
//     preserved, so if the user finally parks at 23:00 instead of 22:00, the
//     hibernation sleeps only until the original 06:00 target.
//   - A validity gate (TimeSynced) prevents any fires until the wall clock
//     has been confirmed plausible by an external signal. The system image
//     seeds the clock with its build timestamp at first boot, so the wall
//     clock alone is not a reliable validity indicator.
//   - A 30-second background poll detects wall-clock jumps (typical when GPS
//     time sync converges) and re-evaluates both the cron next-fire and any
//     pending deferred wake target.
type Scheduler struct {
	logger *log.Logger

	mu             sync.Mutex
	cronExpr       string
	duration       time.Duration
	enabled        bool
	timeSynced     bool
	vehicleStandby bool
	pendingWake    *time.Time // wall-clock target if a fire is deferred to next standby
	lastFired      time.Time  // dedup guard for cron-driven duplicate ticks

	cronEngine *cron.Cron
	cronEntry  cron.EntryID

	maxSeconds func() uint32
	onFire     func(wakeSeconds uint32)

	monitorCtx    context.Context
	monitorCancel context.CancelFunc
	lastWall      time.Time
	lastMono      time.Time
}

// NewScheduler constructs a scheduler. The onFire callback dispatches
// EvPowerHibernateFor and is invoked from the cron goroutine; the implementation
// is expected to be thread-safe. maxSeconds returns the runtime cap on a single
// wake-timer request and is queried each time onFire is invoked, so changes to
// pm.wake-timer-max-seconds take effect immediately.
func NewScheduler(logger *log.Logger, maxSeconds func() uint32, onFire func(wakeSeconds uint32)) *Scheduler {
	return &Scheduler{
		logger:     logger,
		cronEngine: cron.New(cron.WithParser(cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow))),
		maxSeconds: maxSeconds,
		onFire:     onFire,
	}
}

// Start begins clock-jump monitoring. The scheduler does not fire any
// occurrences until SetEnabled(true), SetCron(...), SetDuration(...), AND
// SetTimeSynced(true) have all been satisfied.
func (s *Scheduler) Start(ctx context.Context) {
	s.cronEngine.Start()

	s.mu.Lock()
	s.monitorCtx, s.monitorCancel = context.WithCancel(ctx)
	s.lastWall = time.Now()
	s.lastMono = s.lastWall
	monitorCtx := s.monitorCtx
	s.mu.Unlock()

	go s.monitorLoop(monitorCtx)
}

// Close stops the cron engine and monitor loop.
func (s *Scheduler) Close() {
	s.mu.Lock()
	if s.monitorCancel != nil {
		s.monitorCancel()
		s.monitorCancel = nil
	}
	s.mu.Unlock()
	s.cronEngine.Stop()
}

// SetEnabled toggles whether the schedule is allowed to fire.
func (s *Scheduler) SetEnabled(enabled bool) {
	s.mu.Lock()
	if s.enabled == enabled {
		s.mu.Unlock()
		return
	}
	s.enabled = enabled
	s.mu.Unlock()
	s.logger.Printf("Scheduled hibernation enabled=%v", enabled)
	s.rebuild()
}

// SetCron updates the cron expression. An empty or invalid expression
// effectively disables the schedule.
func (s *Scheduler) SetCron(expr string) {
	s.mu.Lock()
	if s.cronExpr == expr {
		s.mu.Unlock()
		return
	}
	s.cronExpr = expr
	s.mu.Unlock()
	s.logger.Printf("Scheduled hibernation cron expression set to %q", expr)
	s.rebuild()
}

// SetDuration updates the wake-by duration applied at fire time.
func (s *Scheduler) SetDuration(d time.Duration) {
	s.mu.Lock()
	if s.duration == d {
		s.mu.Unlock()
		return
	}
	s.duration = d
	s.mu.Unlock()
	s.logger.Printf("Scheduled hibernation duration set to %v", d)
	// No rebuild needed: duration is read at fire time.
}

// SetTimeSynced is a latch. While false, no cron fires are dispatched and any
// deferred pending fire stays armed. The first true call rebuilds the cron
// schedule so its next-fire reflects the corrected wall clock; further calls
// (including false) are ignored — once chrony has bootstrapped the clock, it
// stays trustworthy for the session even if the underlying source (GPS fix,
// NTP reachability) drops out later.
func (s *Scheduler) SetTimeSynced(synced bool) {
	if !synced {
		return
	}
	s.mu.Lock()
	if s.timeSynced {
		s.mu.Unlock()
		return
	}
	s.timeSynced = true
	pending := s.pendingWake
	s.mu.Unlock()
	s.logger.Printf("Scheduled hibernation time-synced=true (latched)")

	// Rebuild so next-fire is computed against the now-synced wall clock.
	s.rebuild()
	// Drop pending fires whose target wake is now in the past (the sync may
	// have stepped the clock far forward).
	if pending != nil && time.Now().After(*pending) {
		s.logger.Printf("Dropping deferred wake whose target is now in the past after time sync")
		s.mu.Lock()
		s.pendingWake = nil
		s.mu.Unlock()
	}
}

// OnVehicleStateChanged tells the scheduler the current vehicle state. When
// the vehicle transitions into standby with a pending deferred wake target,
// the hibernation request is dispatched with the remaining seconds.
func (s *Scheduler) OnVehicleStateChanged(state string) {
	standby := state == "stand-by" || state == "standby" || state == "parked"

	s.mu.Lock()
	prev := s.vehicleStandby
	s.vehicleStandby = standby
	pending := s.pendingWake
	enabled := s.enabled && s.timeSynced
	maxSec := s.maxSecondsLocked()
	s.mu.Unlock()

	if !standby || prev == standby || pending == nil || !enabled {
		return
	}
	wakeSec := pendingWakeSeconds(*pending, time.Now(), maxSec)
	if wakeSec == 0 {
		s.logger.Printf("Pending wake target already in the past on standby; dropping")
		s.mu.Lock()
		s.pendingWake = nil
		s.mu.Unlock()
		return
	}
	s.logger.Printf("Vehicle entered standby with pending wake; firing hibernate-for %d seconds", wakeSec)
	s.mu.Lock()
	s.pendingWake = nil
	s.mu.Unlock()
	if s.onFire != nil {
		s.onFire(wakeSec)
	}
}

// fire is invoked by the cron engine when the schedule triggers.
func (s *Scheduler) fire() {
	s.mu.Lock()
	if !s.enabled || !s.timeSynced || s.duration <= 0 {
		reasons := make([]string, 0, 3)
		if !s.enabled {
			reasons = append(reasons, "disabled")
		}
		if !s.timeSynced {
			reasons = append(reasons, "wall clock not time-synced")
		}
		if s.duration <= 0 {
			reasons = append(reasons, "duration is zero")
		}
		s.mu.Unlock()
		s.logger.Printf("Scheduled hibernation fire suppressed: %s", strings.Join(reasons, ", "))
		return
	}
	now := time.Now()
	// Dedup: cron should not double-fire the same minute, but guard anyway.
	if !s.lastFired.IsZero() && now.Sub(s.lastFired) < 30*time.Second {
		s.mu.Unlock()
		return
	}
	s.lastFired = now
	target := now.Add(s.duration)
	if s.vehicleStandby {
		s.mu.Unlock()
		wakeSec := uint32(s.duration / time.Second)
		if cap := s.maxSeconds(); wakeSec > cap {
			wakeSec = cap
		}
		s.logger.Printf("Scheduled hibernation firing immediately: wake in %d s", wakeSec)
		if s.onFire != nil {
			s.onFire(wakeSec)
		}
		return
	}
	s.pendingWake = &target
	s.mu.Unlock()
	s.logger.Printf("Scheduled hibernation deferred: vehicle not in standby; target wake at %v", target.Format(time.RFC3339))
}

// rebuild replaces the cron entry under the current configuration. Called
// whenever cron/enabled/timeSynced change.
func (s *Scheduler) rebuild() {
	s.mu.Lock()
	if s.cronEntry != 0 {
		s.cronEngine.Remove(s.cronEntry)
		s.cronEntry = 0
	}
	if !s.enabled || s.cronExpr == "" {
		s.mu.Unlock()
		return
	}
	expr := s.cronExpr
	s.mu.Unlock()

	id, err := s.cronEngine.AddFunc(expr, s.fire)
	if err != nil {
		s.logger.Printf("Invalid cron expression %q: %v", expr, err)
		return
	}
	s.mu.Lock()
	s.cronEntry = id
	s.mu.Unlock()
	s.logger.Printf("Scheduled hibernation cron entry installed: %q", expr)
}

// monitorLoop watches for wall-clock jumps (typically GPS sync) and rebuilds
// the cron schedule so future fires reflect the corrected clock.
func (s *Scheduler) monitorLoop(ctx context.Context) {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			wallNow := time.Now()
			s.mu.Lock()
			monoElapsed := time.Since(s.lastMono)
			wallElapsed := wallNow.Sub(s.lastWall)
			s.lastWall = wallNow
			s.lastMono = time.Now()
			pending := s.pendingWake
			s.mu.Unlock()

			jump := wallElapsed - monoElapsed
			if jump < -60*time.Second || jump > 60*time.Second {
				s.logger.Printf("Detected wall-clock jump (delta=%v); rebuilding schedule", jump)
				s.rebuild()
				if pending != nil && wallNow.After(*pending) {
					s.logger.Printf("Pending wake target is now in the past after clock jump; dropping")
					s.mu.Lock()
					s.pendingWake = nil
					s.mu.Unlock()
				}
			}
		}
	}
}

func (s *Scheduler) maxSecondsLocked() uint32 {
	if s.maxSeconds == nil {
		return 0
	}
	return s.maxSeconds()
}

// pendingWakeSeconds returns the number of seconds until target, clamped to
// the cap. Returns 0 if target is already in the past.
func pendingWakeSeconds(target, now time.Time, cap uint32) uint32 {
	if !target.After(now) {
		return 0
	}
	secs := uint64(target.Sub(now) / time.Second)
	if cap > 0 && secs > uint64(cap) {
		return cap
	}
	if secs == 0 {
		// Sub-second remaining: round up to 1 so we still arm.
		return 1
	}
	return uint32(secs)
}
