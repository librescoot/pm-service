package clockcontrol

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"time"
)

const MaxRequest = 4096

func Serve(ctx context.Context, path string, e *Engine) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	defer ln.Close()
	defer os.Remove(path)
	if err = os.Chmod(path, 0600); err != nil {
		return err
	}
	go func() { <-ctx.Done(); ln.Close() }()
	slots := make(chan struct{}, 16)
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case slots <- struct{}{}:
			go func() { defer func() { <-slots }(); handle(c, e) }()
		default:
			c.Close()
		}
	}
}
func handle(c net.Conn, e *Engine) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(4 * time.Second))
	rd := bufio.NewReader(io.LimitReader(c, MaxRequest+1))
	line, err := rd.ReadBytes('\n')
	if err != nil || len(line) > MaxRequest {
		return
	}
	var r Request
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	err = dec.Decode(&r)
	if err == nil {
		var extra any
		if dec.Decode(&extra) != io.EOF {
			err = errors.New("trailing JSON")
		}
	}
	if err == nil {
		switch r.Op {
		case "status":
		case "submit":
			err = e.Submit(r)
		case "prepare":
			ctx, cancel := context.WithTimeout(context.Background(), 3500*time.Millisecond)
			err = e.Prepare(ctx)
			cancel()
		case "abort":
			err = e.Abort()
		case "resume":
			err = e.Resume(r.BootID, r.Generation)
		default:
			err = errors.New("unknown operation")
		}
	}
	out := Response{OK: err == nil, Quality: e.Status()}
	if err != nil {
		out.Error = err.Error()
	}
	_ = json.NewEncoder(c).Encode(out)
}
