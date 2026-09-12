package service

import (
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestShouldBlockSuspendForRemoteAccess(t *testing.T) {
	tests := []struct {
		name              string
		suspendWhenOnline bool
		status            string
		statusKnown       bool
		withinGrace       bool
		want              bool
	}{
		{name: "connected blocks", status: "connected", statusKnown: true, want: true},
		{name: "disconnected allows", status: "disconnected", statusKnown: true},
		{name: "absent allows after grace", statusKnown: true},
		{name: "grace blocks disconnected", status: "disconnected", statusKnown: true, withinGrace: true, want: true},
		{name: "read error blocks", statusKnown: false, want: true},
		{name: "setting on allows connected", suspendWhenOnline: true, status: "connected", statusKnown: true},
		{name: "setting on bypasses grace", suspendWhenOnline: true, withinGrace: true},
		{name: "setting on bypasses read error", suspendWhenOnline: true, statusKnown: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldBlockSuspendForRemoteAccess(tt.suspendWhenOnline, tt.status, tt.statusKnown, tt.withinGrace)
			if got != tt.want {
				t.Errorf("shouldBlockSuspendForRemoteAccess() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestClockPlausible(t *testing.T) {
	build := time.Unix(1_700_000_000, 0)
	tests := []struct {
		name  string
		now   time.Time
		build time.Time
		want  bool
	}{
		{name: "no build timestamp disables check", now: time.Unix(0, 0), want: true},
		{name: "after build", now: build.Add(time.Hour), build: build, want: true},
		{name: "at build", now: build, build: build, want: true},
		{name: "before build (fake-hwclock seed)", now: build.Add(-time.Hour), build: build, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clockPlausible(tt.now, tt.build); got != tt.want {
				t.Errorf("clockPlausible(%v, %v) = %v, want %v", tt.now, tt.build, got, tt.want)
			}
		})
	}
}

func TestLoadBuildTimestamp(t *testing.T) {
	dir := t.TempDir()

	good := filepath.Join(dir, "build-timestamp")
	if err := os.WriteFile(good, []byte("1700000000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := loadBuildTimestamp(good); !got.Equal(time.Unix(1_700_000_000, 0)) {
		t.Errorf("loadBuildTimestamp(good) = %v, want 1700000000", got)
	}

	bad := filepath.Join(dir, "bad")
	if err := os.WriteFile(bad, []byte("not-a-number"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := loadBuildTimestamp(bad); !got.IsZero() {
		t.Errorf("loadBuildTimestamp(bad) = %v, want zero", got)
	}

	if got := loadBuildTimestamp(filepath.Join(dir, "missing")); !got.IsZero() {
		t.Errorf("loadBuildTimestamp(missing) = %v, want zero", got)
	}
}

func TestTimeSyncEvidence(t *testing.T) {
	floor := time.Unix(1_700_000_000, 0)
	tests := []struct {
		name  string
		gps   bool
		ntp   bool
		now   time.Time
		floor time.Time
		want  bool
	}{
		{name: "gps only", gps: true, now: floor.Add(time.Hour), floor: floor, want: true},
		{name: "ntp only", ntp: true, now: floor.Add(time.Hour), floor: floor, want: true},
		{name: "neither", now: floor.Add(time.Hour), floor: floor, want: false},
		{name: "gps but implausible", gps: true, now: floor.Add(-time.Hour), floor: floor, want: false},
		{name: "ntp but implausible", ntp: true, now: floor.Add(-time.Hour), floor: floor, want: false},
		{name: "no floor disables plausibility", gps: true, now: time.Unix(0, 0), want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := timeSyncEvidence(tt.gps, tt.ntp, tt.now, tt.floor); got != tt.want {
				t.Errorf("timeSyncEvidence(%v, %v, %v, %v) = %v, want %v",
					tt.gps, tt.ntp, tt.now, tt.floor, got, tt.want)
			}
		})
	}
}

func TestLoadHWClock(t *testing.T) {
	dir := t.TempDir()

	older := filepath.Join(dir, "older")
	if err := os.WriteFile(older, []byte("2026-01-01 00:00:00\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	newer := filepath.Join(dir, "newer")
	if err := os.WriteFile(newer, []byte("2026-06-01 12:00:00\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "bad")
	if err := os.WriteFile(bad, []byte("nonsense"), 0o644); err != nil {
		t.Fatal(err)
	}

	want := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	if got := loadHWClock(older, newer, bad, filepath.Join(dir, "missing")); !got.Equal(want) {
		t.Errorf("loadHWClock = %v, want %v", got, want)
	}
	if got := loadHWClock(); !got.IsZero() {
		t.Errorf("loadHWClock() = %v, want zero", got)
	}
}

func TestLaterOf(t *testing.T) {
	a := time.Unix(100, 0)
	b := time.Unix(200, 0)
	if got := laterOf(a, b); !got.Equal(b) {
		t.Errorf("laterOf(a,b) = %v, want %v", got, b)
	}
	if got := laterOf(b, a); !got.Equal(b) {
		t.Errorf("laterOf(b,a) = %v, want %v", got, b)
	}
	if got := laterOf(time.Time{}, a); !got.Equal(a) {
		t.Errorf("laterOf(zero,a) = %v, want %v", got, a)
	}
}

func TestParseChronyTracking(t *testing.T) {
	out := "Reference ID    : C0A80101 (192.168.1.1)\n" +
		"Stratum         : 2\n" +
		"System time     : 0.000000123 seconds fast of NTP time\n" +
		"Leap status     : Normal\n"

	refid, leap := parseChronyTracking(out)
	if refid != "C0A80101" {
		t.Errorf("refid = %q, want C0A80101", refid)
	}
	if leap != "Normal" {
		t.Errorf("leap = %q, want Normal", leap)
	}
	if refid, leap := parseChronyTracking(""); refid != "" || leap != "" {
		t.Errorf("parseChronyTracking(\"\") = (%q,%q), want empty", refid, leap)
	}
}

func TestIsPseudoRefID(t *testing.T) {
	tests := []struct {
		refid string
		want  bool
	}{
		{"", true},
		{"7F7F0101", true},     // local mode
		{"7F7F0100", true},     // manual mode
		{"7f7f0101", true},     // case-insensitive
		{"127.127.1.1", true},  // dotted form
		{"00000000", true},     // none
		{"0.0.0.0", true},      // none, dotted
		{"C0A80101", false},    // 192.168.1.1
		{"192.168.1.1", false}, // dotted real
		{"ntp1.example.com", false},
	}
	for _, tt := range tests {
		t.Run(tt.refid, func(t *testing.T) {
			if got := isPseudoRefID(tt.refid); got != tt.want {
				t.Errorf("isPseudoRefID(%q) = %v, want %v", tt.refid, got, tt.want)
			}
		})
	}
}

// writeFakeChronyc creates an executable that prints the given output, so the
// ntpSynced exec path can be exercised without chrony installed.
func writeFakeChronyc(t *testing.T, output string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "chronyc")
	script := "#!/bin/sh\ncat <<'EOF'\n" + output + "\nEOF\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNTPSynced(t *testing.T) {
	orig := chronycBinary
	t.Cleanup(func() { chronycBinary = orig })

	tests := []struct {
		name string
		out  string
		want bool
	}{
		{"real source", "Reference ID    : C0A80101 (192.168.1.1)\nStratum         : 2\nLeap status     : Normal\n", true},
		{"local pseudo-ref", "Reference ID    : 7F7F0101 ()\nStratum         : 1\nLeap status     : Normal\n", false},
		{"manual pseudo-ref", "Reference ID    : 7F7F0100 ()\nLeap status     : Normal\n", false},
		{"not synchronised", "Reference ID    : 00000000 ()\nLeap status     : Not synchronised\n", false},
		{"empty output", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chronycBinary = writeFakeChronyc(t, tt.out)
			if got := ntpSynced(); got != tt.want {
				t.Errorf("ntpSynced() = %v, want %v", got, tt.want)
			}
		})
	}

	t.Run("missing binary", func(t *testing.T) {
		chronycBinary = filepath.Join(t.TempDir(), "missing")
		if ntpSynced() {
			t.Error("ntpSynced() = true with no chronyc binary")
		}
	})
}

func TestScheduledHibernateMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "marker")

	if present, err := readAndClearScheduledHibernateMarker(path); err != nil || present {
		t.Fatalf("absent marker = (%v, %v), want (false, nil)", present, err)
	}
	if err := writeScheduledHibernateMarker(path); err != nil {
		t.Fatal(err)
	}
	if present, err := readAndClearScheduledHibernateMarker(path); err != nil || !present {
		t.Fatalf("present marker = (%v, %v), want (true, nil)", present, err)
	}
	if present, _ := readAndClearScheduledHibernateMarker(path); present {
		t.Error("marker not cleared by the first read")
	}
}

func TestClockGuard(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	boot0 := 5 * time.Minute

	t.Run("untrusted is invalid", func(t *testing.T) {
		var g clockGuard
		if g.valid(base, boot0) {
			t.Fatal("untrusted guard reported the clock valid")
		}
	})
	t.Run("normal progress is valid", func(t *testing.T) {
		var g clockGuard
		g.mark(base, boot0)
		if !g.valid(base.Add(10*time.Minute), boot0+10*time.Minute) {
			t.Fatal("guard rejected a clock that kept pace")
		}
	})
	t.Run("frozen across suspend is invalid", func(t *testing.T) {
		var g clockGuard
		g.mark(base, boot0)
		if g.valid(base, boot0+2*time.Hour) {
			t.Fatal("guard accepted a clock frozen across a 2h suspend")
		}
	})
	t.Run("drift within slack is valid", func(t *testing.T) {
		var g clockGuard
		g.mark(base, boot0)
		if !g.valid(base.Add(time.Hour-clockGuardSlack/2), boot0+time.Hour) {
			t.Fatal("guard rejected drift within slack")
		}
	})
	t.Run("behind beyond slack is invalid", func(t *testing.T) {
		var g clockGuard
		g.mark(base, boot0)
		if g.valid(base.Add(time.Hour-2*clockGuardSlack), boot0+time.Hour) {
			t.Fatal("guard accepted a clock behind by more than slack")
		}
	})
	t.Run("monotonic readings do not hide a freeze", func(t *testing.T) {
		var g clockGuard
		now := time.Now()
		g.mark(now, boot0)
		if g.valid(now, boot0+2*time.Hour) {
			t.Fatal("guard accepted a frozen clock because monotonic readings were compared")
		}
	})
}

func TestBootElapsed(t *testing.T) {
	d, err := bootElapsed()
	if err != nil {
		t.Skipf("/proc/uptime unavailable: %v", err)
	}
	if d <= 0 {
		t.Fatalf("bootElapsed() = %v, want > 0", d)
	}
}

func TestClockValidUnreadableBootTime(t *testing.T) {
	s := &Service{
		logger:        log.New(io.Discard, "", 0),
		bootElapsedFn: func() (time.Duration, error) { return 0, errors.New("boom") },
	}
	if s.clockValid() {
		t.Fatal("clockValid() = true with an unreadable boot time")
	}
}
