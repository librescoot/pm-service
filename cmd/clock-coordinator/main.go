package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/librescoot/pm-service/internal/clockcontrol"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
func run() error {
	dir := flag.String("run-dir", "/run/clock-coordinator", "root-controlled runtime directory")
	refdir := flag.String("refclock-dir", "/run/chrony", "chrony SOCK directory")
	wait := flag.Duration("wait", 0, "wait up to this duration for local quality, then continue")
	flag.Parse()
	if flag.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	if *wait < 0 || *wait > 5*time.Minute {
		return errors.New("invalid wait duration")
	}
	socket := filepath.Join(*dir, "control.sock")
	if *wait > 0 {
		deadline := time.Now().Add(*wait)
		for time.Now().Before(deadline) {
			c, err := net.DialTimeout("unix", socket, 200*time.Millisecond)
			if err == nil {
				_ = c.SetDeadline(time.Now().Add(200 * time.Millisecond))
				_, _ = fmt.Fprintln(c, `{"op":"status"}`)
				var r clockcontrol.Response
				err = json.NewDecoder(bufio.NewReader(c)).Decode(&r)
				c.Close()
				if err == nil && r.OK && r.Quality.Source != "" && r.Quality.Source != "unknown" {
					return nil
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		return nil
	}
	if os.Geteuid() != 0 {
		return errors.New("daemon requires root")
	}
	if err := os.MkdirAll(*dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(*dir)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm()&0022 != 0 || st.Uid != 0 {
		return errors.New("runtime directory must be root-owned and not group/world writable")
	}
	lock, err := clockcontrol.AcquireLock(filepath.Join(*dir, "owner.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return err
	}
	engine, err := clockcontrol.NewEngine(filepath.Join(*dir, "state.json"), strings.TrimSpace(string(boot)), clockcontrol.SystemClock{}, clockcontrol.Chronyc{}, clockcontrol.SockRefclock{Dir: *refdir})
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	go engine.Run(ctx)
	return clockcontrol.Serve(ctx, socket, engine)
}
