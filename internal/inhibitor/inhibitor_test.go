package inhibitor

import (
	"io"
	"log"
	"net"
	"testing"
)

func TestHibernateTargetInhibitorMatrix(t *testing.T) {
	for _, target := range []string{"hibernate", "hibernate-manual", "hibernate-timer", "hibernate-for", "reboot", "suspend", "run", "unknown"} {
		for _, kind := range []InhibitorType{TypeBlock, TypeSuspendOnly, TypeDelay} {
			for _, connectionBased := range []bool{false, true} {
				m := &Manager{logger: log.New(io.Discard, "", 0)}
				inh := &Inhibitor{Type: kind}
				if connectionBased {
					m.inhibitors = map[net.Conn]*Inhibitor{nil: inh}
				} else {
					m.manualInhibitors = []*Inhibitor{inh}
				}
				want := kind == TypeBlock || (kind == TypeSuspendOnly && !IsHibernatePath(target))
				if got := m.HasBlockingInhibitors(target); got != want {
					t.Errorf("target=%s kind=%s connections=%v got=%v want=%v", target, kind, connectionBased, got, want)
				}
			}
		}
	}
}
