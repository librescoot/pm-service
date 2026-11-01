package clockcontrol

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

type Clock interface {
	Now() (time.Time, time.Duration, error)
}
type SystemClock struct{}

func (SystemClock) Now() (time.Time, time.Duration, error) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts); err != nil {
		return time.Time{}, 0, err
	}
	return time.Now(), time.Duration(ts.Sec)*time.Second + time.Duration(ts.Nsec), nil
}

type Tracking struct {
	Source      string
	External    bool
	Age         time.Duration
	Residual    float64
	Uncertainty float64
	SkewPPM     float64
	Valid       bool
}
type Chrony interface {
	Tracking(context.Context, time.Time) (Tracking, error)
	StartStep(context.Context) (<-chan error, error)
}
type Chronyc struct{ Path string }

func (c Chronyc) command(ctx context.Context, args ...string) *exec.Cmd {
	path := c.Path
	if path == "" {
		path = "chronyc"
	}
	return exec.CommandContext(ctx, path, args...)
}

type boundedOutput struct {
	bytes.Buffer
	overflow bool
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	if b.Len()+n > 4096 {
		b.overflow = true
		return n, nil
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}
func (c Chronyc) StartStep(ctx context.Context) (<-chan error, error) {
	cmd := c.command(ctx, "makestep")
	var output boundedOutput
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		if err == nil && (output.overflow || strings.TrimSpace(output.String()) != "200 OK") {
			err = errors.New("unconfirmed makestep response")
		}
		done <- err
	}()
	return done, nil
}
func (c Chronyc) Tracking(ctx context.Context, now time.Time) (Tracking, error) {
	var q Tracking
	out, err := c.command(ctx, "-n", "-c", "tracking").Output()
	if err != nil {
		return q, err
	}
	f := strings.Split(strings.TrimSpace(string(out)), ",")
	if len(f) != 14 || f[13] != "Normal" {
		return q, errors.New("chrony not synchronized")
	}
	ref, err := strconv.ParseUint(f[0], 16, 32)
	if err != nil || len(f[0]) != 8 || ref == 0 {
		return q, errors.New("invalid reference")
	}
	stratum, err := strconv.Atoi(f[2])
	if err != nil || stratum < 1 || stratum > 15 {
		return q, errors.New("invalid stratum")
	}
	parse := func(s string) (float64, error) {
		v, e := strconv.ParseFloat(s, 64)
		if e != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, errors.New("invalid tracking number")
		}
		return v, nil
	}
	stamp, e := parse(f[3])
	if e != nil {
		return q, e
	}
	residual, e := parse(f[4])
	if e != nil {
		return q, e
	}
	skew, e := parse(f[9])
	if e != nil || skew < 0 {
		return q, errors.New("invalid skew")
	}
	delay, e := parse(f[10])
	if e != nil || delay < 0 {
		return q, errors.New("invalid delay")
	}
	dispersion, e := parse(f[11])
	if e != nil || dispersion < 0 {
		return q, errors.New("invalid dispersion")
	}
	// Tracking's reference timestamp is in chrony's corrected timescale.
	age := float64(now.UnixNano())/1e9 + residual - stamp
	if age < -5 || age > 7200 {
		return q, errors.New("stale tracking")
	}
	if age < 0 {
		age = 0
	}
	q = Tracking{Source: "local", Age: time.Duration(age * float64(time.Second)), Residual: residual, Uncertainty: delay/2 + dispersion, SkewPPM: skew, Valid: true}
	sources, err := c.command(ctx, "-n", "-c", "sources").Output()
	if err != nil {
		return Tracking{}, err
	}
	found := false
	for _, line := range strings.Split(strings.TrimSpace(string(sources)), "\n") {
		cols := strings.Split(line, ",")
		if len(cols) < 3 || cols[1] != "*" {
			continue
		}
		if found {
			return Tracking{}, errors.New("multiple selected sources")
		}
		found = true
		switch {
		case cols[0] == "#" && cols[2] == "GPS1" && f[0] == "47505331":
			q.Source = SourceGPS
			q.External = true
		case cols[0] == "#" && cols[2] == "NRF1" && f[0] == "4E524631":
			q.Source = SourceNRF
		case cols[0] == "#" && cols[2] == "USR1" && f[0] == "55535231":
			q.Source = SourceUser
		case cols[0] == "^" && ref>>24 != 127 && f[0] != "4C4F434C" && cols[2] == f[1]:
			peer, err := c.command(ctx, "-n", "ntpdata", cols[2]).Output()
			if err != nil {
				return Tracking{}, err
			}
			var peerID uint64
			var foundID bool
			for _, line := range strings.Split(string(peer), "\n") {
				key, value, ok := strings.Cut(line, ":")
				if !ok || strings.TrimSpace(key) != "Reference ID" {
					continue
				}
				fields := strings.Fields(value)
				if len(fields) == 0 || len(fields[0]) != 8 || foundID {
					return Tracking{}, errors.New("invalid peer reference ID")
				}
				peerID, err = strconv.ParseUint(fields[0], 16, 32)
				if err != nil {
					return Tracking{}, err
				}
				foundID = true
			}
			if !foundID {
				return Tracking{}, errors.New("missing peer reference ID")
			}
			if peerID != 0 && peerID>>24 != 127 && peerID != 0x4c4f434c && peerID != 0x4e524631 && peerID != 0x55535231 {
				q.Source = "ntp"
				q.External = true
			} else if peerID != 0 {
				q.Source = SourceNetworkHoldover
			}
		}
	}
	if !found {
		return Tracking{}, errors.New("no selected source")
	}
	return q, nil
}

type Refclock interface {
	Send(source string, at time.Time, offset float64) error
}
type SockRefclock struct{ Dir string }

func (s SockRefclock) Send(source string, at time.Time, offset float64) error {
	names := map[string]string{SourceNRF: "nrf.sock", SourceGPS: "gps.sock", SourceUser: "user.sock"}
	name, ok := names[source]
	if !ok {
		return errors.New("unknown source")
	}
	var order binary.ByteOrder = binary.LittleEndian
	var probe uint16 = 1
	if *(*byte)(unsafe.Pointer(&probe)) == 0 {
		order = binary.BigEndian
	}
	b, err := EncodeSOCK(at, offset, 8, order)
	if err != nil {
		return err
	}
	c, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: filepath.Join(s.Dir, name), Net: "unixgram"})
	if err != nil {
		return err
	}
	defer c.Close()
	if err = c.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		return err
	}
	n, err := c.Write(b)
	if err == nil && n != len(b) {
		return fmt.Errorf("short datagram: %d", n)
	}
	return err
}
