package main

// A log of the last session, for when something goes wrong: each update's
// size and timings, slow writes to the pane, and errors. It lives at
// ~/.local/state/rex-glance/last.log and is replaced on every look.

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type sessionLog struct {
	mu    sync.Mutex
	f     *os.File
	start time.Time
}

var glog = &sessionLog{}

func openLog(host string) {
	dir := filepath.Join(os.Getenv("HOME"), ".local", "state", "rex-glance")
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "last.log"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	glog.f, glog.start = f, time.Now()
	glog.printf("look %s", host)
}

func (l *sessionLog) printf(format string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return
	}
	fmt.Fprintf(l.f, "%8.3fs  %s\n", time.Since(l.start).Seconds(), fmt.Sprintf(format, a...))
}
