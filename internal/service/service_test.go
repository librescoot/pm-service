package service

import "testing"

func TestInternetConnectivityDrivesSuspendGuard(t *testing.T) {
	tests := []struct {
		name              string
		suspendWhenOnline bool
		connectivity      string
		wantOnline        bool
		wantBlocked       bool
	}{
		{name: "connected blocks suspend", connectivity: "connected", wantOnline: true, wantBlocked: true},
		{name: "disconnected allows suspend", connectivity: "disconnected"},
		{name: "disabled allows suspend", connectivity: "disabled"},
		{name: "no-sim allows suspend", connectivity: "no-sim"},
		{name: "denied allows suspend", connectivity: "denied"},
		{name: "failed allows suspend", connectivity: "failed"},
		{name: "unknown allows suspend", connectivity: ""},
		{
			name:              "connected allows suspend when setting is on",
			suspendWhenOnline: true,
			connectivity:      "connected",
			wantOnline:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Service{suspendWhenOnline: tt.suspendWhenOnline}
			if err := s.onInternetConnectivityChanged(tt.connectivity); err != nil {
				t.Fatalf("onInternetConnectivityChanged(%q) returned %v", tt.connectivity, err)
			}
			if s.online != tt.wantOnline {
				t.Errorf("online = %v after connectivity %q, want %v", s.online, tt.connectivity, tt.wantOnline)
			}
			if got := s.suspendBlockedWhileOnline(); got != tt.wantBlocked {
				t.Errorf("suspendBlockedWhileOnline() = %v, want %v", got, tt.wantBlocked)
			}
		})
	}
}

// online must follow connectivity in both directions: a session that goes away
// (or stops being reported at all) has to clear it again, otherwise a scooter
// that was briefly online never suspends.
func TestOnlineStartsFalseUntilConnectivitySeen(t *testing.T) {
	s := &Service{}
	if s.online {
		t.Fatal("online should default to false")
	}
	if err := s.onInternetConnectivityChanged("disconnected"); err != nil {
		t.Fatalf("onInternetConnectivityChanged returned %v", err)
	}
	if s.online {
		t.Error("online should stay false while connectivity is disconnected")
	}
	if err := s.onInternetConnectivityChanged("connected"); err != nil {
		t.Fatalf("onInternetConnectivityChanged returned %v", err)
	}
	if !s.online {
		t.Error("online should be true once connectivity reports connected")
	}
	if err := s.onInternetConnectivityChanged(""); err != nil {
		t.Fatalf("onInternetConnectivityChanged returned %v", err)
	}
	if s.online {
		t.Error("online should clear when connectivity becomes unknown")
	}
}
