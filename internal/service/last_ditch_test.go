package service

import (
	"bytes"
	"io"
	"log"
	"strings"
	"testing"
	"time"
)

// lastDitchTriggeredLocked decides whether to cut power to the scooter, so the
// condition matrix is worth pinning down. The grace window is set in the past
// unless a case is exercising it.
func newLastDitchService(t *testing.T) *Service {
	t.Helper()
	return &Service{
		logger:              log.New(io.Discard, "", 0),
		lastDitchEnabled:    defaultLastDitchHibernateEnabled,
		lastDitchGraceUntil: time.Now().Add(-time.Minute),
	}
}

func TestLastDitchTriggered(t *testing.T) {
	tests := []struct {
		name                 string
		b0Present, b1Present bool
		b0Charge, b1Charge   int
		cbb                  int
		auxLow               bool
		want                 bool
	}{
		// A usable main battery is the whole reason not to hibernate.
		{"main battery present and charged", true, false, 80, -1, 0, true, false},
		{"second slot carries it", false, true, -1, 80, 0, true, false},

		// Both slots gone is necessary but never sufficient on its own.
		{"both slots gone, reserves fine", false, false, 0, 0, 90, false, false},
		{"both slots gone, CBB low", false, false, 0, 0, 10, false, true},
		{"both slots gone, aux low", false, false, 0, 0, 90, true, true},
		{"both slots gone, both reserves low", false, false, 0, 0, 10, true, true},

		// Present but flat counts as missing: a 0% pack cannot move the scooter.
		{"present but flat", true, true, 0, 0, 10, false, true},

		// Unknown must not be read as empty, or a scooter hibernates because a
		// reading has not arrived yet.
		{"CBB unknown suppresses its arm", false, false, 0, 0, -1, false, false},
		{"CBB unknown but aux latched still fires", false, false, 0, 0, -1, true, true},

		// Exactly at the threshold is not below it.
		{"CBB exactly at threshold", false, false, 0, 0, lastDitchHibernateCBBThreshold, false, false},
		{"CBB one under threshold", false, false, 0, 0, lastDitchHibernateCBBThreshold - 1, false, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newLastDitchService(t)
			s.battery0Present, s.battery1Present = tt.b0Present, tt.b1Present
			s.battery0Charge, s.battery1Charge = tt.b0Charge, tt.b1Charge
			s.cbBatteryCharge = tt.cbb
			s.auxLowLatched = tt.auxLow

			s.lastDitchMu.Lock()
			got := s.lastDitchTriggeredLocked()
			s.lastDitchMu.Unlock()

			if got != tt.want {
				t.Errorf("lastDitchTriggeredLocked() = %v, want %v", got, tt.want)
			}
		})
	}
}

// The boot grace exists so a wake that races the first telemetry sync cannot
// immediately hibernate again.
func TestLastDitchSuppressedWithinBootGrace(t *testing.T) {
	s := newLastDitchService(t)
	s.battery0Present, s.battery1Present = false, false
	s.battery0Charge, s.battery1Charge = 0, 0
	s.cbBatteryCharge = 0
	s.lastDitchGraceUntil = time.Now().Add(time.Minute)

	s.lastDitchMu.Lock()
	got := s.lastDitchTriggeredLocked()
	s.lastDitchMu.Unlock()

	if got {
		t.Fatal("triggered inside the boot grace window")
	}

	// Same inputs, grace expired.
	s.lastDitchGraceUntil = time.Now().Add(-time.Second)
	s.lastDitchMu.Lock()
	got = s.lastDitchTriggeredLocked()
	s.lastDitchMu.Unlock()

	if !got {
		t.Fatal("did not trigger after the grace window expired")
	}
}

// Defaults must not fire before the first sync: presence defaults to true and
// the charges to unknown precisely so a fresh boot stays quiet.
func TestLastDitchDoesNotFireOnStartupDefaults(t *testing.T) {
	s := newLastDitchService(t)
	s.battery0Present, s.battery1Present = true, true
	s.battery0Charge, s.battery1Charge = -1, -1
	s.cbBatteryCharge = -1

	s.lastDitchMu.Lock()
	defer s.lastDitchMu.Unlock()
	if s.lastDitchTriggeredLocked() {
		t.Fatal("triggered on startup defaults")
	}
}

func TestLastDitchSettingDefaultsEnabled(t *testing.T) {
	if !defaultLastDitchHibernateEnabled {
		t.Fatal("missing setting must preserve production-enabled behavior")
	}
}

func TestLastDitchSettingFalseSuppressesTrigger(t *testing.T) {
	s := newLastDitchService(t)
	s.battery0Present, s.battery1Present = false, false
	s.battery0Charge, s.battery1Charge = 0, 0
	s.cbBatteryCharge = 0

	if err := s.onLastDitchHibernateEnabledSetting("false"); err != nil {
		t.Fatal(err)
	}
	s.lastDitchMu.Lock()
	triggered := s.lastDitchTriggeredLocked()
	s.lastDitchMu.Unlock()
	if triggered {
		t.Fatal("disabled setting did not suppress automatic last-ditch trigger")
	}
}

func TestLastDitchSettingInvalidFailsEnabledAndLogs(t *testing.T) {
	s := newLastDitchService(t)
	if err := s.onLastDitchHibernateEnabledSetting("false"); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	s.logger = log.New(&logs, "", 0)

	if err := s.onLastDitchHibernateEnabledSetting("definitely"); err != nil {
		t.Fatal(err)
	}
	s.lastDitchMu.Lock()
	enabled := s.lastDitchEnabled
	s.lastDitchMu.Unlock()
	if !enabled {
		t.Fatal("invalid setting did not fail safe to enabled")
	}
	if !strings.Contains(logs.String(), "Invalid pm.last-ditch-hibernate-enabled") {
		t.Fatalf("invalid setting was not logged: %q", logs.String())
	}
}

func TestLastDitchSettingRuntimeToggles(t *testing.T) {
	s := newLastDitchService(t)
	for _, test := range []struct {
		value string
		want  bool
	}{
		{value: "false", want: false},
		{value: "true", want: true},
		{value: " false ", want: false},
		{value: " true ", want: true},
	} {
		if err := s.onLastDitchHibernateEnabledSetting(test.value); err != nil {
			t.Fatalf("setting %q: %v", test.value, err)
		}
		s.lastDitchMu.Lock()
		got := s.lastDitchEnabled
		s.lastDitchMu.Unlock()
		if got != test.want {
			t.Errorf("setting %q produced %t, want %t", test.value, got, test.want)
		}
	}
}
