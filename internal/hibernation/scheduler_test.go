package hibernation

import (
	"context"
	"io"
	"log"
	"strings"
	"testing"
	"time"
)

func newTestScheduler(onFire func(uint32)) *Scheduler {
	return NewScheduler(
		log.New(io.Discard, "", 0),
		func() uint32 { return 3600 },
		onFire,
	)
}

func newRunningTestScheduler(t *testing.T, onFire func(uint32)) *Scheduler {
	t.Helper()
	s := newTestScheduler(onFire)
	s.Start(context.Background())
	t.Cleanup(s.Close)
	return s
}

// TestImmediateFireClearsPendingWake guards against a deferred target leaking
// into a later standby transition after the schedule has already fired.
func TestImmediateFireClearsPendingWake(t *testing.T) {
	fired := 0
	s := newTestScheduler(func(uint32) { fired++ })
	s.SetEnabled(true)
	s.SetCron("0 22 * * *")
	s.SetDuration(8 * time.Hour)
	s.SetTimeSynced(true)

	target := time.Now().Add(time.Hour)
	s.mu.Lock()
	s.vehicleStandby = true
	s.pendingWake = &target
	s.mu.Unlock()

	s.fire()

	if fired != 1 {
		t.Fatalf("onFire called %d times, want 1", fired)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pendingWake != nil {
		t.Fatal("pendingWake not cleared by an immediate fire")
	}
}

// TestImmediateFireIgnoresZeroCap: a maxSeconds callback returning 0 means
// "no cap", not "clamp to zero".
func TestImmediateFireIgnoresZeroCap(t *testing.T) {
	var got uint32
	s := NewScheduler(
		log.New(io.Discard, "", 0),
		func() uint32 { return 0 },
		func(sec uint32) { got = sec },
	)
	s.SetEnabled(true)
	s.SetCron("0 22 * * *")
	s.SetDuration(8 * time.Hour)
	s.SetTimeSynced(true)
	s.mu.Lock()
	s.vehicleStandby = true
	s.mu.Unlock()

	s.fire()

	want := uint32((8 * time.Hour) / time.Second)
	if got != want {
		t.Fatalf("onFire(%d), want %d", got, want)
	}
}

// TestDisabledStandbyTransitionCancelsPending: a deferred fire that arrives
// while the schedule is disabled must not survive to the next standby entry.
func TestDisabledStandbyTransitionCancelsPending(t *testing.T) {
	s := newTestScheduler(nil)
	s.SetEnabled(false)
	s.SetCron("0 22 * * *")
	s.SetDuration(8 * time.Hour)

	target := time.Now().Add(time.Hour)
	s.mu.Lock()
	s.pendingWake = &target
	s.mu.Unlock()

	s.OnVehicleStateChanged("stand-by")

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pendingWake != nil {
		t.Fatal("pendingWake survived entering standby while disabled")
	}
}

// TestConcurrentRebuildKeepsSingleCronEntry catches the unlocked window in
// rebuild() where two goroutines can each install a cron entry.
func TestConcurrentRebuildKeepsSingleCronEntry(t *testing.T) {
	s := newTestScheduler(nil)
	s.SetEnabled(true)
	s.SetCron("0 22 * * *")

	for round := 0; round < 10; round++ {
		done := make(chan struct{})
		for i := 0; i < 100; i++ {
			go func() {
				defer func() { done <- struct{}{} }()
				s.rebuild()
			}()
		}
		for i := 0; i < 100; i++ {
			<-done
		}
		if entries := s.cronEngine.Entries(); len(entries) != 1 {
			t.Fatalf("round %d: cron has %d entries after concurrent rebuild, want 1", round, len(entries))
		}
	}
}

// TestDetectWallJumpStripsMonotonic verifies detection works with plain wall
// times. TestDetectWallJumpWithMonotonicReadings is the regression for the
// original formula, where Sub silently returned a monotonic delta.
func TestDetectWallJumpStripsMonotonic(t *testing.T) {
	lastWall := time.Date(2026, 1, 1, 22, 0, 0, 0, time.UTC)
	wallNow := lastWall.Add(2 * time.Hour)
	lastMono := time.Now()
	monoNow := lastMono.Add(50 * time.Millisecond)

	got := detectWallJump(lastWall, lastMono, wallNow, monoNow)
	if got < 2*time.Hour-time.Second || got > 2*time.Hour {
		t.Fatalf("detectWallJump() = %v, want ~2h", got)
	}
}

func TestDetectWallJumpWithMonotonicReadings(t *testing.T) {
	// Add preserves the monotonic reading, so a naive wallNow.Sub(lastWall)
	// would return the monotonic delta and hide the 2h step entirely.
	lastWall := time.Now()
	wallNow := lastWall.Add(2 * time.Hour)
	lastMono := time.Now()
	monoNow := lastMono.Add(time.Second)

	if got := detectWallJump(lastWall, lastMono, wallNow, monoNow); got <= time.Hour {
		t.Fatalf("detectWallJump() = %v, want ~2h; monotonic reading not stripped", got)
	}
}

func TestCheckClockJumpStoresWallWithoutMonotonic(t *testing.T) {
	s := newRunningTestScheduler(t, nil)
	s.checkClockJump()

	s.mu.Lock()
	lastWall := s.lastWall
	s.mu.Unlock()
	if strings.Contains(lastWall.String(), "m=") {
		t.Fatalf("lastWall retained a monotonic reading: %s", lastWall)
	}
}

func TestCheckClockJumpDropsPendingAfterForwardJump(t *testing.T) {
	s := newRunningTestScheduler(t, nil)
	target := time.Now().Add(-time.Minute)
	s.mu.Lock()
	s.pendingWake = &target
	s.lastWall = time.Now().Round(0).Add(-2 * time.Hour) // clock stepped forward 2h
	s.lastMono = time.Now()
	s.mu.Unlock()

	s.checkClockJump()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pendingWake != nil {
		t.Fatal("pendingWake survived a forward wall-clock jump past its target")
	}
}

func TestCheckClockJumpKeepsPendingOnNormalTick(t *testing.T) {
	s := newRunningTestScheduler(t, nil)
	target := time.Now().Add(8 * time.Hour)
	s.mu.Lock()
	s.pendingWake = &target
	s.mu.Unlock()

	s.checkClockJump()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pendingWake == nil {
		t.Fatal("normal tick dropped a pending wake that is still in the future")
	}
}

// TestCatchUpArmsRecentMissedOccurrence: an occurrence that fell during this
// process's lifetime while the time-sync gate was closed should still hibernate
// if its wake-by target is in the future.
func TestCatchUpArmsRecentMissedOccurrence(t *testing.T) {
	fired := 0
	s := newTestScheduler(func(uint32) { fired++ })
	s.SetEnabled(true)
	s.SetCron("*/15 * * * *")
	s.SetDuration(time.Hour)

	quarterStart := time.Now().Round(0).Truncate(15 * time.Minute)
	s.mu.Lock()
	s.timeSynced = true
	s.startedWall = quarterStart.Add(-time.Minute)
	s.mu.Unlock()

	s.catchUpMissed()

	if fired != 0 {
		t.Fatalf("catch-up fired while not in standby: %d", fired)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pendingWake == nil {
		t.Fatal("catch-up did not arm a pending wake for the missed occurrence")
	}
	if !s.pendingWake.After(time.Now()) {
		t.Fatalf("catch-up armed a past target: %v", s.pendingWake)
	}
}

func TestCatchUpIgnoresOccurrenceBeforeProcessStart(t *testing.T) {
	s := newTestScheduler(nil)
	s.SetEnabled(true)
	s.SetCron("*/15 * * * *")
	s.SetDuration(time.Hour)

	quarterStart := time.Now().Round(0).Truncate(15 * time.Minute)
	s.mu.Lock()
	s.timeSynced = true
	s.startedWall = quarterStart.Add(time.Minute) // occurrence predates this process
	s.mu.Unlock()

	s.catchUpMissed()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pendingWake != nil {
		t.Fatal("catch-up armed a wake for an occurrence that predates the process")
	}
}

func TestCatchUpFiresImmediatelyInStandby(t *testing.T) {
	fired := 0
	s := newTestScheduler(func(uint32) { fired++ })
	s.SetEnabled(true)
	s.SetCron("*/15 * * * *")
	s.SetDuration(time.Hour)

	quarterStart := time.Now().Round(0).Truncate(15 * time.Minute)
	s.mu.Lock()
	s.timeSynced = true
	s.startedWall = quarterStart.Add(-time.Minute)
	s.vehicleStandby = true
	s.mu.Unlock()

	s.catchUpMissed()

	if fired != 1 {
		t.Fatalf("onFire called %d times, want 1", fired)
	}
}

func TestValidateCronSchedule(t *testing.T) {
	tests := []struct {
		expr string
		ok   bool
	}{
		{"0 22 * * *", true},
		{"*/15 * * * *", true},
		{"*/30 * * * *", true},
		{"0 */6 * * *", true},
		{"0 6,18 * * *", true},
		{"0 22 * * 1", true},
		{"* * * * *", false},
		{"*/5 * * * *", false},
		{"0,10 6 * * *", false},
		{"0 22 1 1 *", true},
		{"not a cron", false},
	}
	for _, tt := range tests {
		t.Run(tt.expr, func(t *testing.T) {
			err := validateCronSchedule(tt.expr)
			if tt.ok && err != nil {
				t.Errorf("validateCronSchedule(%q) = %v, want nil", tt.expr, err)
			}
			if !tt.ok && err == nil {
				t.Errorf("validateCronSchedule(%q) = nil, want error", tt.expr)
			}
		})
	}
}

// TestSetCronRejectsFrequentSchedule: a too-frequent expression must be
// ignored, leaving the previous schedule in place.
func TestSetCronRejectsFrequentSchedule(t *testing.T) {
	s := newTestScheduler(nil)
	s.SetEnabled(true)
	s.SetCron("0 22 * * *")
	s.SetCron("* * * * *")

	s.mu.Lock()
	expr := s.cronExpr
	s.mu.Unlock()
	if expr != "0 22 * * *" {
		t.Fatalf("cronExpr = %q, want the previously accepted expression", expr)
	}
	if entries := s.cronEngine.Entries(); len(entries) != 1 {
		t.Fatalf("cron has %d entries, want 1", len(entries))
	}
}

// TestSuppressScheduledFiresFor guarantees a wake from a scheduled hibernation
// cannot immediately re-hibernate on a frequent schedule.
func TestSuppressScheduledFiresFor(t *testing.T) {
	fired := 0
	s := newRunningTestScheduler(t, func(uint32) { fired++ })
	s.SetEnabled(true)
	s.SetCron("0 22 * * *")
	s.SetDuration(8 * time.Hour)
	s.SetTimeSynced(true)
	s.mu.Lock()
	s.vehicleStandby = true
	s.mu.Unlock()

	s.SuppressScheduledFiresFor(time.Hour)
	s.fire()
	if fired != 0 {
		t.Fatalf("fire ran during the startup cooldown: %d", fired)
	}

	// Once the cooldown expires, fires resume.
	s.mu.Lock()
	s.suppressUntil = time.Now().Add(-time.Second)
	s.mu.Unlock()
	s.fire()
	if fired != 1 {
		t.Fatalf("fire suppressed after the startup cooldown expired: %d", fired)
	}
}
