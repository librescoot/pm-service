package service

import (
	"encoding/json"
	"fmt"
	"math"
	"net"
	"os"
	"strings"
	"time"

	"github.com/librescoot/pm-service/internal/clockcontrol"
	"golang.org/x/sys/unix"
)

var clockSocket = "/run/clock-coordinator/control.sock"
var clockBootID = "/proc/sys/kernel/random/boot_id"

func coordinatorRequest(req clockcontrol.Request) (clockcontrol.Quality, error) {
	var resp clockcontrol.Response
	var started, finished unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &started); err != nil {
		return resp.Quality, err
	}
	c, err := net.DialTimeout("unix", clockSocket, time.Second)
	if err != nil {
		return resp.Quality, err
	}
	defer c.Close()
	if err = c.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return resp.Quality, err
	}
	if err = json.NewEncoder(c).Encode(req); err != nil {
		return resp.Quality, err
	}
	var raw struct {
		OK      bool            `json:"ok"`
		Error   string          `json:"error"`
		Quality json.RawMessage `json:"quality"`
	}
	if err = json.NewDecoder(c).Decode(&raw); err != nil {
		return resp.Quality, fmt.Errorf("unknown coordinator outcome: %w", err)
	}
	if err = json.Unmarshal(raw.Quality, &resp.Quality); err != nil {
		return resp.Quality, err
	}
	resp.OK, resp.Error = raw.OK, raw.Error
	if req.Op == "status" {
		var fields map[string]json.RawMessage
		if err = json.Unmarshal(raw.Quality, &fields); err != nil {
			return resp.Quality, err
		}
		for _, name := range []string{"boot_id", "generation", "source", "external", "age_ns", "residual_seconds", "uncertainty_seconds", "outcome", "preparing"} {
			if v, ok := fields[name]; !ok || string(v) == "null" {
				return resp.Quality, fmt.Errorf("incomplete coordinator status: %s", name)
			}
		}
	}
	if req.Op == "status" {
		if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &finished); err != nil {
			return resp.Quality, err
		}
		elapsed := finished.Nano() - started.Nano()
		if elapsed < 0 || elapsed > int64(30*time.Second) || resp.Quality.AgeNS < 0 || resp.Quality.AgeNS > int64(30*time.Second)-elapsed {
			return resp.Quality, fmt.Errorf("stale coordinator response")
		}
		resp.Quality.AgeNS += elapsed
	}
	if !resp.OK {
		return resp.Quality, fmt.Errorf("coordinator rejected %s: %s", req.Op, resp.Error)
	}
	return resp.Quality, nil
}
func trustedClockQuality(q clockcontrol.Quality, boot string) bool {
	return boot != "" && q.BootID == boot && q.Generation != 0 && !q.Preparing && q.External && (q.Source == "gps" || q.Source == "ntp") && q.AgeNS >= 0 && q.AgeNS <= int64(30*time.Second) && q.Outcome != "" && q.Outcome != "unknown" && q.Outcome != "in-flight" && !math.IsNaN(q.ResidualSeconds) && !math.IsInf(q.ResidualSeconds, 0) && !math.IsNaN(q.UncertaintySeconds) && !math.IsInf(q.UncertaintySeconds, 0) && q.UncertaintySeconds >= 0 && math.Abs(q.ResidualSeconds)+q.UncertaintySeconds <= 5
}
func currentClockQuality() (clockcontrol.Quality, error) {
	q, err := coordinatorRequest(clockcontrol.Request{Op: "status"})
	if err != nil {
		return q, err
	}
	boot, err := os.ReadFile(clockBootID)
	if err != nil {
		return q, err
	}
	if !trustedClockQuality(q, strings.TrimSpace(string(boot))) {
		return q, fmt.Errorf("no fresh external clock quality")
	}
	return q, nil
}

func recoverClockPreparation() error {
	q, err := coordinatorRequest(clockcontrol.Request{Op: "status"})
	if err != nil {
		return err
	}
	if !q.Preparing {
		return nil
	}
	boot, err := os.ReadFile(clockBootID)
	if err != nil {
		return err
	}
	if q.BootID == "" || q.BootID != strings.TrimSpace(string(boot)) || q.Generation == 0 {
		return fmt.Errorf("invalid clock preparation identity")
	}
	_, err = coordinatorRequest(clockcontrol.Request{Op: "resume", BootID: q.BootID, Generation: q.Generation})
	return err
}
