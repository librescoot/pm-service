package systemd

import (
	"context"
	"fmt"
	"os/exec"
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
		// No system bus (dev environment): degrade to fire-and-forget
		// commands. SuspendAndWaitResume then behaves like IssueCommand.
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

func (c *Client) IssueCommand(command string) error {
	var cmd *exec.Cmd
	switch command {
	case "suspend":
		cmd = exec.Command("systemctl", "suspend")
	case "poweroff":
		cmd = exec.Command("systemctl", "poweroff")
	case "reboot":
		cmd = exec.Command("systemctl", "reboot")
	default:
		return fmt.Errorf("unsupported command: %s", command)
	}

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to execute %s: %v", command, err)
	}

	return nil
}

// SuspendAndWaitResume requests a suspend and blocks until the system has
// actually gone down and come back up, so the caller can read a fresh wakeup
// reason. enterTimeout bounds the wait for the descent (PrepareForSleep(true));
// if logind never starts the sleep, the suspend is treated as failed. The wait
// for resume is bounded only by ctx: while suspended this process is frozen,
// and on thaw the PrepareForSleep(false) broadcast is already queued.
//
// Without a logind signal subscription (no system bus), this degrades to the
// plain IssueCommand behavior and returns once the job is enqueued.
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
