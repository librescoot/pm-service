package systemd

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestNotifyReady(t *testing.T) {
	for _, abstract := range []bool{false, true} {
		t.Run(strconv.FormatBool(abstract), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "notify")
			if abstract {
				path = "@pm-ready-" + strconv.Itoa(os.Getpid())
			}
			listener, err := net.ListenPacket("unixgram", path)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			t.Setenv("NOTIFY_SOCKET", path)
			if err := NotifyReady(); err != nil {
				t.Fatal(err)
			}
			if err := listener.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			message := make([]byte, 128)
			n, _, err := listener.ReadFrom(message)
			if err != nil || string(message[:n]) != "READY=1" {
				t.Fatalf("notification = %q, %v", message[:n], err)
			}
		})
	}
}

func TestNotifyReadyWithoutSystemd(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	if err := NotifyReady(); err != nil {
		t.Fatal(err)
	}
}

func TestNotifyReadyReportsFailure(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", filepath.Join(t.TempDir(), "missing"))
	if err := NotifyReady(); err == nil {
		t.Fatal("notification failure was ignored")
	}
}
