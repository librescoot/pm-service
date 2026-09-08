package service

import "testing"

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
