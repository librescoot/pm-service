package systemd

import (
	"fmt"

	"github.com/godbus/dbus/v5"
)

type Client struct {
	conn *dbus.Conn
}

func NewClient() (*Client, error) {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return nil, fmt.Errorf("failed to connect to system bus: %v", err)
	}

	return &Client{
		conn: conn,
	}, nil
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

	obj := c.conn.Object("org.freedesktop.login1", "/org/freedesktop/login1")
	call := obj.Call("org.freedesktop.login1.Manager."+method, 0, false)
	if call.Err != nil {
		return fmt.Errorf("failed to call %s: %v", method, call.Err)
	}

	return nil
}

func (c *Client) Close() error {
	return c.conn.Close()
}
