package hibernation

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// FireCooldown is the minimum wall-clock gap enforced between two scheduled
// hibernation fires in the same process. It both deduplicates same-minute cron
// ticks and suppresses runaway expressions like "* * * * *" or "*/5 * * * *"
// while the scooter is awake. It does not survive a hibernation: the process is
// powered off, so lastFired resets on the next boot.
const FireCooldown = 15 * time.Minute

// MinScheduleInterval is the shortest gap allowed between consecutive
// occurrences of a scheduled hibernation cron expression. Because FireCooldown
// only survives within one process, a schedule that fires more often than this
// cannot be told apart from a hibernate/wake loop across poweroffs, so it is
// rejected at configuration time instead.
const MinScheduleInterval = FireCooldown

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
	lastFired      time.Time  // cooldown guard: suppress fires within FireCooldown of the last one
	suppressUntil  time.Time  // monotonic instant before which scheduled fires are held off

	cronEngine *cron.Cron
	cronEntry  cron.EntryID

	maxSeconds func() uint32
	onFire     func(wakeSeconds uint32)

	monitorCtx    context.Context
	monitorCancel context.CancelFunc
	lastWall      time.Time
	lastMono      time.Time
	startedWall   time.Time // wall reading (monotonic stripped) at Start
	startedMono   time.Time
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
	now := time.Now()
	s.lastWall = now.Round(0) // strip monotonic so Sub compares wall time
	s.lastMono = now
	s.startedWall = now.Round(0)
	s.startedMono = now
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

// SetCron updates the cron expression. An empty expression disables the
// schedule. Expressions whose consecutive occurrences are closer than
// MinScheduleInterval are rejected (the previous expression stays in effect),
// as are invalid expressions.
func (s *Scheduler) SetCron(expr string) {
	if expr != "" {
		if err := validateCronSchedule(expr); err != nil {
			s.logger.Printf("Ignoring pm.scheduled-hibernate-cron=%q: %v", expr, err)
			return
		}
	}
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

// SuppressScheduledFiresFor holds off scheduled fires for d of process uptime.
// It is called after waking from a scheduled hibernation so a schedule that
// fires again immediately cannot put the scooter into a hibernate/wake loop.
// The guard is deliberately monotonic: the wall clock is not trustworthy until
// the time-sync gate opens, but CLOCK_MONOTONIC always is.
func (s *Scheduler) SuppressScheduledFiresFor(d time.Duration) {
	s.mu.Lock()
	base := s.startedMono
	if base.IsZero() {
		base = time.Now()
	}
	s.suppressUntil = base.Add(d)
	s.mu.Unlock()
	s.logger.Printf("Scheduled hibernation suppressed for %v of uptime after a scheduled wake", d)
}

// validateCronSchedule parses expr and rejects any two consecutive occurrences
// that are closer together than MinScheduleInterval. A bounded sample is
// enough: low-frequency expressions (yearly included) are caught by their
// widest gap, and a frequent expression trips on its first pair.
func validateCronSchedule(expr string) error {
	sched, err := cron.ParseStandard(expr)
	if err != nil {
		return err
	}
	const samples = 1000
	prev := sched.Next(time.Now())
	for i := 0; i < samples; i++ {
		next := sched.Next(prev)
		if next.IsZero() {
			break
		}
		if gap := next.Sub(prev); gap < MinScheduleInterval {
			return fmt.Errorf("consecutive occurrences %s and %s are only %v apart (minimum %v)",
				prev.Format(time.RFC3339), next.Format(time.RFC3339), gap, MinScheduleInterval)
		}
		prev = next
	}
	return nil
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
	// Honor an occurrence that elapsed while the gate was closed, as long as
	// the process was already running past it and the wake-by target is still
	// in the future.
	s.catchUpMissed()
}

// OnVehicleStateChanged tells the scheduler the current vehicle state. When
// the vehicle transitions into standby with a pending deferred wake target,
// the hibernation request is dispatched with the remaining seconds.
//
// Only "stand-by" (locked, idle) counts as standby for the purpose of
// firing a scheduled hibernation. "parked" (unlocked, idle near the
// scooter) defers like ready-to-drive so the user has to explicitly lock
// before scheduled hibernation will hit them.
func (s *Scheduler) OnVehicleStateChanged(state string) {
	standby := state == "stand-by" || state == "standby"

	s.mu.Lock()
	prev := s.vehicleStandby
	s.vehicleStandby = standby

	var pending time.Time
	havePending := false
	if standby && !prev && s.pendingWake != nil {
		if !s.enabled || !s.timeSynced {
			// Entering standby while the schedule is inactive cancels the
			// deferred intent. Without this a stale target would fire on the
			// next standby transition, long after the schedule that armed it
			// was turned off.
			s.logger.Printf("Vehicle entered standby while scheduled hibernation is inactive; dropping pending wake")
			s.pendingWake = nil
		} else {
			// Consume under the same lock as the read so a concurrent fire()
			// cannot dispatch the same target twice.
			pending = *s.pendingWake
			havePending = true
			s.pendingWake = nil
		}
	}
	maxSec := s.maxSecondsLocked()
	s.mu.Unlock()

	if !standby || prev == standby || !havePending {
		return
	}
	wakeSec := pendingWakeSeconds(pending, time.Now(), maxSec)
	if wakeSec == 0 {
		s.logger.Printf("Pending wake target already in the past on standby; dropping")
		return
	}
	s.logger.Printf("Vehicle entered standby with pending wake; firing hibernate-for %d seconds", wakeSec)
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
	if !s.suppressUntil.IsZero() && now.Before(s.suppressUntil) {
		s.mu.Unlock()
		s.logger.Printf("Scheduled hibernation fire suppressed: within startup cooldown after a scheduled wake")
		return
	}
	// Cooldown: suppress fires that come within FireCooldown of the previous
	// one. This both dedupes same-minute cron ticks and prevents pathological
	// expressions (e.g. "* * * * *") from re-hibernating the scooter the
	// moment it wakes back up.
	if !s.lastFired.IsZero() && now.Sub(s.lastFired) < FireCooldown {
		s.mu.Unlock()
		s.logger.Printf("Scheduled hibernation fire suppressed: within %v cooldown of last fire", FireCooldown)
		return
	}
	s.lastFired = now
	target := now.Add(s.duration)
	if s.vehicleStandby {
		// An immediate fire supersedes any deferred target.
		s.pendingWake = nil
		s.mu.Unlock()
		wakeSec := uint32(s.duration / time.Second)
		if wakeSec == 0 {
			// Sub-second duration: arm the nRF for at least one second rather
			// than sending a 0 that the service drops.
			wakeSec = 1
		}
		if cap := s.maxSeconds(); cap > 0 && wakeSec > cap {
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
	defer s.mu.Unlock()

	if s.cronEntry != 0 {
		s.cronEngine.Remove(s.cronEntry)
		s.cronEntry = 0
	}
	if !s.enabled || s.cronExpr == "" {
		return
	}
	id, err := s.cronEngine.AddFunc(s.cronExpr, s.fire)
	if err != nil {
		s.logger.Printf("Invalid cron expression %q: %v", s.cronExpr, err)
		return
	}
	s.cronEntry = id
	s.logger.Printf("Scheduled hibernation cron entry installed: %q", s.cronExpr)
}

// wallJumpThreshold is the wall/monotonic divergence that counts as a clock
// step warranting a schedule rebuild.
const wallJumpThreshold = 60 * time.Second

// detectWallJump reports how far the wall clock has drifted from where it would
// be if only the monotonic clock had advanced. Both wall readings are stripped
// of their monotonic readings before subtraction: when two time.Time values
// carry monotonic readings, Time.Sub returns the monotonic difference, which
// makes an external CLOCK_REALTIME step invisible.
func detectWallJump(lastWall, lastMono, wallNow, monoNow time.Time) time.Duration {
	wallElapsed := wallNow.Round(0).Sub(lastWall.Round(0))
	monoElapsed := monoNow.Sub(lastMono)
	return wallElapsed - monoElapsed
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
			s.checkClockJump()
		}
	}
}

// checkClockJump compares the wall clock against the monotonic clock since the
// previous tick and rebuilds the schedule if it moved by more than
// wallJumpThreshold.
func (s *Scheduler) checkClockJump() {
	wallNow := time.Now()
	monoNow := time.Now()

	s.mu.Lock()
	lastWall := s.lastWall
	lastMono := s.lastMono
	s.lastWall = wallNow.Round(0)
	s.lastMono = monoNow
	s.mu.Unlock()

	jump := detectWallJump(lastWall, lastMono, wallNow, monoNow)
	if jump >= -wallJumpThreshold && jump <= wallJumpThreshold {
		return
	}
	s.logger.Printf("Detected wall-clock jump (delta=%v); rebuilding schedule", jump)
	s.rebuild()

	s.mu.Lock()
	pending := s.pendingWake
	s.mu.Unlock()
	if pending != nil && wallNow.After(*pending) {
		s.logger.Printf("Pending wake target is now in the past after clock jump; dropping")
		s.mu.Lock()
		if s.pendingWake != nil && !s.pendingWake.After(wallNow) {
			s.pendingWake = nil
		}
		s.mu.Unlock()
	}
}

// catchUpWindow bounds how far back catchUpMissed looks for a missed
// occurrence.
const catchUpWindow = 48 * time.Hour

// catchUpMissed honors a scheduled occurrence that elapsed during this
// process's lifetime while timeSynced was still false, provided the resulting
// wake-by target is still in the future. It is called when the clock first
// becomes valid.
//
// Occurrences older than the process start are ignored. The fake-hwclock can
// restore a stale wall clock at boot and then step it forward on sync, so an
// occurrence that appears to be "in the past" after the step did not belong to
// this run and must not trigger a hibernate.
func (s *Scheduler) catchUpMissed() {
	s.mu.Lock()
	if !s.enabled || !s.timeSynced || s.cronExpr == "" || s.duration <= 0 ||
		(!s.suppressUntil.IsZero() && time.Now().Before(s.suppressUntil)) {
		s.mu.Unlock()
		return
	}
	expr := s.cronExpr
	duration := s.duration
	startedWall := s.startedWall
	maxSec := s.maxSecondsLocked()
	standby := s.vehicleStandby
	s.mu.Unlock()

	sched, err := cron.ParseStandard(expr)
	if err != nil {
		return
	}
	now := time.Now()
	prev, ok := previousOccurrence(sched, now, catchUpWindow)
	if !ok || !prev.After(startedWall) {
		return
	}
	target := prev.Add(duration)
	if !target.After(now) {
		// The wake-by target has already passed; hibernating now would defeat
		// the schedule.
		return
	}

	if standby {
		wakeSec := pendingWakeSeconds(target, now, maxSec)
		if wakeSec == 0 {
			return
		}
		s.logger.Printf("Catching up missed scheduled hibernation (fired %s); firing immediately: wake in %d s",
			prev.Format(time.RFC3339), wakeSec)
		s.mu.Lock()
		s.lastFired = now
		s.pendingWake = nil
		s.mu.Unlock()
		if s.onFire != nil {
			s.onFire(wakeSec)
		}
		return
	}

	s.mu.Lock()
	if !s.enabled || !s.timeSynced || s.cronExpr != expr {
		s.mu.Unlock()
		return
	}
	s.pendingWake = &target
	s.mu.Unlock()
	s.logger.Printf("Catching up missed scheduled hibernation (fired %s); deferred until standby, target wake %s",
		prev.Format(time.RFC3339), target.Format(time.RFC3339))
}

// previousOccurrence returns the latest schedule activation at or before now
// that is no older than now-lookback.
func previousOccurrence(sched cron.Schedule, now time.Time, lookback time.Duration) (time.Time, bool) {
	start := now.Add(-lookback).Add(-time.Second)
	var last time.Time
	next := sched.Next(start)
	for i := 0; i < 100000 && !next.IsZero() && !next.After(now); i++ {
		last = next
		next = sched.Next(next)
	}
	if last.IsZero() {
		return time.Time{}, false
	}
	return last, true
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
