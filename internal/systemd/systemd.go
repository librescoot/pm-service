package systemd

import (
	"context"
	"fmt"
	"time"

	"github.com/godbus/dbus/v5"
)

// Client issues power commands and tracks the system's sleep cycle via
// logind's PrepareForSleep signal. `systemctl suspend` returns when the
// suspend job is enqueued, not when the system wakes, so treating its return
// as "we just woke up" reads a stale wakeup reason and cycles the FSM while
// the kernel is still on its way down. PrepareForSleep(true) marks the actual
// descent, PrepareForSleep(false) the actual resume.
type Client struct {
	conn    *dbus.Conn
	signals chan *dbus.Signal
}

func NewClient() (*Client, error) {
	c := &Client{}

	conn, err := dbus.SystemBus()
	if err != nil {
		// No system bus (dev environment): the client still comes up, but
		// every power command errors, since logind is the only issue path.
		return c, nil
	}

	if err := conn.AddMatchSignal(
		dbus.WithMatchInterface("org.freedesktop.login1.Manager"),
		dbus.WithMatchMember("PrepareForSleep"),
		dbus.WithMatchObjectPath("/org/freedesktop/login1"),
	); err != nil {
		conn.Close()
		return c, nil
	}

	c.conn = conn
	c.signals = make(chan *dbus.Signal, 16)
	conn.Signal(c.signals)

	return c, nil
}

// logind D-Bus addressing. We issue power transitions as login1 Manager
// method calls rather than shelling out to systemctl: logind is the component
// that actually writes /sys/power/state and emits the PrepareForSleep signal
// we already wait on, so issuing through it keeps the descent and the wait on
// the same actor.
const (
	login1Dest    = "org.freedesktop.login1"
	login1Path    = "/org/freedesktop/login1"
	login1Manager = "org.freedesktop.login1.Manager"
)

// callLogind invokes a login1 Manager method (Suspend/PowerOff/Reboot) with
// interactive=false. pm-service runs as root, so logind authorizes these
// without a polkit prompt. Errors if there is no system bus (dev environment).
func (c *Client) callLogind(method string) error {
	if c.conn == nil {
		return fmt.Errorf("no system bus: cannot call logind %s", method)
	}
	obj := c.conn.Object(login1Dest, dbus.ObjectPath(login1Path))
	if call := obj.Call(login1Manager+"."+method, 0, false); call.Err != nil {
		return fmt.Errorf("logind %s failed: %w", method, call.Err)
	}
	return nil
}

func (c *Client) IssueCommand(command string) error {
	var method string
	switch command {
	case "suspend":
		method = "Suspend"
	case "poweroff":
		method = "PowerOff"
	case "reboot":
		method = "Reboot"
	default:
		return fmt.Errorf("unsupported command: %s", command)
	}

	return c.callLogind(method)
}

// SuspendAndWaitResume requests a suspend and blocks until the system has
// actually gone down and come back up, so the caller can read a fresh wakeup
// reason. enterTimeout bounds the wait for the descent (PrepareForSleep(true));
// if logind never starts the sleep, the suspend is treated as failed. The wait
// for resume is bounded only by ctx: while suspended this process is frozen,
// and on thaw the PrepareForSleep(false) broadcast is already queued.
//
// Without a logind signal subscription (no system bus) there is nothing to
// suspend through, so this returns IssueCommand's error.
func (c *Client) SuspendAndWaitResume(ctx context.Context, enterTimeout time.Duration) error {
	if c.signals == nil {
		return c.IssueCommand("suspend")
	}

	// Drop stale signals from a previous cycle so the waits below only see
	// this round's transitions.
	for {
		select {
		case <-c.signals:
			continue
		default:
		}
		break
	}

	if err := c.IssueCommand("suspend"); err != nil {
		return err
	}

	// Phase 1: the descent. logind emits PrepareForSleep(true) before it
	// honors delay inhibitors and writes /sys/power/state.
	enterDeadline := time.NewTimer(enterTimeout)
	defer enterDeadline.Stop()
	suspended := false
	for !suspended {
		select {
		case sig := <-c.signals:
			if suspending, ok := prepareForSleep(sig); ok && suspending {
				suspended = true
			}
		case <-enterDeadline.C:
			return fmt.Errorf("suspend did not start within %v", enterTimeout)
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// Phase 2: the resume.
	for {
		select {
		case sig := <-c.signals:
			if suspending, ok := prepareForSleep(sig); ok && !suspending {
				return nil
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func prepareForSleep(sig *dbus.Signal) (suspending bool, ok bool) {
	if sig == nil || sig.Name != "org.freedesktop.login1.Manager.PrepareForSleep" || len(sig.Body) != 1 {
		return false, false
	}
	v, ok := sig.Body[0].(bool)
	return v, ok
}

func (c *Client) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}
