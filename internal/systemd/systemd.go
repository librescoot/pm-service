package systemd

import (
	"fmt"
	"os/exec"
)

type Client struct{}

func NewClient() (*Client, error) {
	return &Client{}, nil
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

func (c *Client) Close() error {
	return nil
}
