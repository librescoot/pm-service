package service

import (
	"context"
	"io"
	"log"
	"net"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/librescoot/pm-service/internal/config"
)

func TestReadinessAllowsImmediateInhibitorAcquisition(t *testing.T) {
	dir := t.TempDir()
	notifyPath := filepath.Join(dir, "notify")
	listener, err := net.ListenPacket("unixgram", notifyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv("NOTIFY_SOCKET", notifyPath)
	t.Setenv("DBUS_SYSTEM_BUS_ADDRESS", "unix:path="+filepath.Join(dir, "no-system-bus"))

	redis := miniredis.RunT(t)
	cfg := config.New()
	cfg.RedisHost = redis.Host()
	cfg.RedisPort, _ = strconv.Atoi(redis.Port())
	cfg.SocketPath = filepath.Join(dir, "inhibitor")
	cfg.DefaultState = "run"
	cfg.DryRun = true
	cfg.WakeupSources = nil
	svc, err := New(cfg, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	svc.scheduledMarkerPath = filepath.Join(dir, "scheduled")
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- svc.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-stopped:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("service did not stop")
		}
	}()

	if err := listener.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	message := make([]byte, 128)
	n, _, err := listener.ReadFrom(message)
	if err != nil || string(message[:n]) != "READY=1" {
		t.Fatalf("readiness = %q, %v", message[:n], err)
	}
	conn, err := net.DialTimeout("unix", cfg.SocketPath, time.Second)
	if err != nil {
		t.Fatalf("ready service cannot accept inhibitor: %v", err)
	}
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(conn, message[:1]); err != nil {
		t.Fatalf("ready service cannot acknowledge inhibitor: %v", err)
	}
	for _, target := range []string{"suspend", "hibernate", "reboot"} {
		if !svc.inhibitorManager.HasBlockingInhibitors(target) {
			t.Errorf("acknowledged inhibitor does not block %s", target)
		}
	}
}
