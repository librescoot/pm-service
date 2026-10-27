package systemd

import (
	"fmt"
	"net"
	"os"
	"time"
)

// NotifyReady is a no-op outside systemd, including foreground development runs.
func NotifyReady() error {
	socket := os.Getenv("NOTIFY_SOCKET")
	if socket == "" {
		return nil
	}
	conn, err := net.DialTimeout("unixgram", socket, time.Second)
	if err != nil {
		return fmt.Errorf("connect to systemd notification socket: %w", err)
	}
	defer conn.Close()
	if err := conn.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		return err
	}
	if _, err := conn.Write([]byte("READY=1")); err != nil {
		return fmt.Errorf("notify systemd readiness: %w", err)
	}
	return nil
}
