package clockcontrol

import (
	"context"
	"encoding/binary"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func TestChronycAdapter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chronyc")
	script := `#!/bin/sh
case "$*" in
 "-n -c tracking") echo '8E84C8F1,192.0.2.1,3,1700000001,2,0,0,0,0,0,0.2,0.1,10,Normal';;
 "-n ntpdata 192.0.2.1") echo 'Reference ID    : 47505300 (GPS)';;
 "-n -c sources") echo '^,*,192.0.2.1,3,6,377,1,0,0,0';;
 makestep) echo '200 OK';;
 *) exit 2;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	c := Chronyc{Path: path}
	q, err := c.Tracking(context.Background(), time.Unix(1700000000, 0))
	if err != nil || !q.External || q.Source != "ntp" || q.Age != time.Second || q.Uncertainty != .2 {
		t.Fatal(q, err)
	}
	done, err := c.StartStep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	for _, reference := range []string{"47505300 (GPS)", "47505331 (GPS1)"} {
		t.Run(reference, func(t *testing.T) {
			externalScript := strings.Replace(script, "47505300 (GPS)", reference, 1)
			if err := os.WriteFile(path, []byte(externalScript), 0700); err != nil {
				t.Fatal(err)
			}
			q, err := c.Tracking(context.Background(), time.Unix(1700000000, 0))
			if err != nil || q.Source != "ntp" || !q.External {
				t.Fatal(q, err)
			}
		})
	}
	for _, reference := range []string{"7F7F0101 (LOCAL)", "4C4F434C (LOCL)", "4E524631 (NRF1)", "55535231 (USR1)"} {
		t.Run(reference, func(t *testing.T) {
			localScript := strings.Replace(script, "47505300 (GPS)", reference, 1)
			if err := os.WriteFile(path, []byte(localScript), 0700); err != nil {
				t.Fatal(err)
			}
			q, err := c.Tracking(context.Background(), time.Unix(1700000000, 0))
			if err != nil || q.Source != SourceNetworkHoldover || q.External {
				t.Fatal(q, err)
			}
		})
	}
	if err = os.WriteFile(path, []byte(strings.Replace(script, "200 OK", "501 Failure", 1)), 0700); err != nil {
		t.Fatal(err)
	}
	done, err = c.StartStep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if <-done == nil {
		t.Fatal("accepted negative chronyc reply")
	}
}
func TestSOCKDatagram(t *testing.T) {
	dir := t.TempDir()
	address := &net.UnixAddr{Name: filepath.Join(dir, "gps.sock"), Net: "unixgram"}
	conn, err := net.ListenUnixgram("unixgram", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err = (SockRefclock{Dir: dir}).Send(SourceGPS, time.Unix(123, 500000000), 3.5); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 64)
	n, _, err := conn.ReadFromUnix(buf)
	if err != nil {
		t.Fatal(err)
	}
	wantSize := 24 + 2*int(unsafe.Sizeof(unix.Timeval{}.Sec))
	if n != wantSize {
		t.Fatalf("native timeval packet size %d, want %d", n, wantSize)
	}
	off := n - 24
	if math.Float64frombits(binary.NativeEndian.Uint64(buf[off:])) != 3.5 || binary.NativeEndian.Uint32(buf[off+20:]) != 0x534f434b {
		t.Fatalf("invalid packet %x", buf[:n])
	}
}
