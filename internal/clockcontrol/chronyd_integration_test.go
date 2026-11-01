package clockcontrol

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Set these binaries to run the isolated, non-clock-changing chronyd test.
func TestChronydSOCKSelection(t *testing.T) {
	daemon, client := os.Getenv("CHRONYD_TEST_BINARY"), os.Getenv("CHRONYC_TEST_BINARY")
	if daemon == "" || client == "" {
		t.Skip("set CHRONYD_TEST_BINARY and CHRONYC_TEST_BINARY")
	}
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		source, id, precision, samples string
		offset                         float64
	}{
		{SourceNRF, "NRF1", "1", "minsamples 1 maxsamples 1", 21600},
		{SourceGPS, "GPS1", "4", "minsamples 4 maxsamples 8", 21600},
		{SourceGPS, "GPS1", "4", "minsamples 4 maxsamples 8", -21600},
	} {
		t.Run(fmt.Sprintf("%s_%g", tc.source, tc.offset), func(t *testing.T) {
			dir, err := os.MkdirTemp("/tmp", "clock-chrony-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			socket := filepath.Join(dir, tc.source+".sock")
			cmdsocket := filepath.Join(dir, "cmd.sock")
			config := fmt.Sprintf("port 0\ncmdport 0\nbindcmdaddress %s\npidfile %s\nmaxdistance 8\nrefclock SOCK %s refid %s precision %s poll 0 filter 1 %s\n", cmdsocket, filepath.Join(dir, "pid"), socket, tc.id, tc.precision, tc.samples)
			conf := filepath.Join(dir, "chrony.conf")
			if err := os.WriteFile(conf, []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 18*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, daemon, "-d", "-x", "-U", "-u", u.Username, "-f", conf)
			var logs strings.Builder
			cmd.Stdout = &logs
			cmd.Stderr = &logs
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
			for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(20 * time.Millisecond) {
				if _, err := os.Stat(socket); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("SOCK unavailable: %s", logs.String())
				}
			}
			wrapper := filepath.Join(dir, "chronyc")
			// Observe dispatch without asking chronyd to change any clock.
			marker := filepath.Join(dir, "steps")
			script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = makestep ]; then echo step >> %q; echo '200 OK'; exit; fi\nexec %q -h %q \"$@\"\n", marker, client, cmdsocket)
			if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			adapter := Chronyc{Path: wrapper}
			ref := SockRefclock{Dir: dir}
			clock := SystemClock{}
			engine, err := NewEngine(filepath.Join(dir, "state"), "test-boot", clock, adapter, ref)
			if err != nil {
				t.Fatal(err)
			}
			submitted := false
			for deadline := time.Now().Add(12 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
				if !submitted || tc.source == SourceGPS {
					now, boot, err := clock.Now()
					if err != nil {
						t.Fatal(err)
					}
					r := Request{BootID: "test-boot", Generation: 1, Source: tc.source, UTCUnixNS: now.Add(time.Duration(tc.offset * float64(time.Second))).UnixNano(), ReceiptBootNS: int64(boot), UncertaintyNS: int64(time.Second)}
					if err := engine.Submit(r); err != nil {
						t.Fatal(err)
					}
					submitted = true
				}
				engine.refresh(ctx)
				if engine.Status().Outcome == "completed" {
					steps, err := os.ReadFile(marker)
					if err != nil || string(steps) != "step\n" {
						t.Fatal("unexpected step dispatch", string(steps), err)
					}
					return
				}
			}
			t.Fatalf("source did not trigger recovery: %+v; %s", engine.Status(), logs.String())
		})
	}
}
