package service

import (
	"encoding/json"
	"github.com/librescoot/pm-service/internal/clockcontrol"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTrustedClockQuality(t *testing.T) {
	q := clockcontrol.Quality{BootID: "boot", Generation: 1, Source: "gps", External: true, Outcome: "completed"}
	if !trustedClockQuality(q, "boot") {
		t.Fatal("valid GPS rejected")
	}
	for _, change := range []func(*clockcontrol.Quality){func(q *clockcontrol.Quality) { q.Source = "nrf" }, func(q *clockcontrol.Quality) { q.Source = "user" }, func(q *clockcontrol.Quality) { q.External = false }, func(q *clockcontrol.Quality) { q.Preparing = true }, func(q *clockcontrol.Quality) { q.Generation = 0 }, func(q *clockcontrol.Quality) { q.AgeNS = int64(31 * time.Second) }, func(q *clockcontrol.Quality) { q.ResidualSeconds = 6 }, func(q *clockcontrol.Quality) { q.Outcome = "unknown" }} {
		x := q
		change(&x)
		if trustedClockQuality(x, "boot") {
			t.Fatalf("accepted %+v", x)
		}
	}
	if trustedClockQuality(q, "other") {
		t.Fatal("accepted different boot")
	}
}
func TestClockGuardReanchorsAfterExternalBackwardCorrection(t *testing.T) {
	var g clockGuard
	first := time.Unix(1900000000, 0)
	g.mark(first, time.Minute)
	corrected := first.Add(-time.Hour)
	if g.valid(corrected, 2*time.Minute) {
		t.Fatal("accepted backward correction before revalidation")
	}
	g.mark(corrected, 2*time.Minute)
	if !g.valid(corrected.Add(time.Minute), 3*time.Minute) {
		t.Fatal("reanchored correction rejected")
	}
}

func TestRecoverClockPreparationUsesPersistedGeneration(t *testing.T) {
	dir := t.TempDir()
	oldSocket, oldBoot := clockSocket, clockBootID
	clockSocket = filepath.Join(dir, "socket")
	clockBootID = filepath.Join(dir, "boot")
	defer func() { clockSocket = oldSocket; clockBootID = oldBoot }()
	if err := os.WriteFile(clockBootID, []byte("boot\n"), 0600); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", clockSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	got := make(chan clockcontrol.Request, 2)
	go func() {
		for i := 0; i < 2; i++ {
			c, err := l.Accept()
			if err != nil {
				return
			}
			var req clockcontrol.Request
			_ = json.NewDecoder(c).Decode(&req)
			got <- req
			if req.Op == "status" {
				_ = json.NewEncoder(c).Encode(clockcontrol.Response{OK: true, Quality: clockcontrol.Quality{BootID: "boot", Generation: 7, Preparing: true, Source: "unknown", Outcome: "available"}})
			} else {
				_ = json.NewEncoder(c).Encode(clockcontrol.Response{OK: true})
			}
			c.Close()
		}
	}()
	if err := recoverClockPreparation(); err != nil {
		t.Fatal(err)
	}
	first, second := <-got, <-got
	if first.Op != "status" || second.Op != "resume" || second.BootID != "boot" || second.Generation != 7 {
		t.Fatalf("requests: %+v %+v", first, second)
	}
}
func TestCoordinatorUnknownOutcomeFailsClosed(t *testing.T) {
	dir := t.TempDir()
	old := clockSocket
	clockSocket = filepath.Join(dir, "socket")
	defer func() { clockSocket = old }()
	l, err := net.Listen("unix", clockSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err == nil {
			defer c.Close()
			var r clockcontrol.Request
			_ = json.NewDecoder(c).Decode(&r)
		}
	}()
	if _, err := coordinatorRequest(clockcontrol.Request{Op: "prepare"}); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unexpected result: %v", err)
	}
}

func TestFutureSavedFallbackDoesNotOverrideExternalClock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-hwclock.data")
	if err := os.WriteFile(path, []byte("2099-01-01 00:00:00\n"), 0600); err != nil {
		t.Fatal(err)
	}
	saved := loadHWClock(path)
	build := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	corrected := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if !saved.After(corrected) {
		t.Fatal("test requires future saved fallback")
	}
	q := clockcontrol.Quality{Source: "ntp", External: true}
	if !calendarClockPlausible(corrected, build, q) {
		t.Fatal("future saved fallback blocked trusted correction")
	}
	q.Source = "nrf"
	if calendarClockPlausible(corrected, build, q) {
		t.Fatal("holdover accepted as external")
	}
}
