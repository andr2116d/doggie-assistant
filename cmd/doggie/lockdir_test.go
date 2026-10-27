package main

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestOneDoggiePerDataDir(t *testing.T) {
	dir := t.TempDir()
	unlock, err := lockDataDir(dir, roleChat)
	if err != nil {
		t.Fatal(err)
	}
	// Who holds it stays readable while it is held.
	_, err = lockDataDir(dir, roleRun)
	var running *runningError
	if !errors.As(err, &running) || running.pid != os.Getpid() || running.role != roleChat {
		t.Fatalf("second lock: %v", err)
	}
	unlock()
	again, err := lockDataDir(dir, roleRun)
	if err != nil {
		t.Fatalf("lock after unlock: %v", err)
	}
	again()
}

func fastHandoverPoll(t *testing.T) {
	old := handoverPoll
	handoverPoll = 10 * time.Millisecond
	t.Cleanup(func() { handoverPoll = old })
}

// A request is seen once, and its file is gone.
func TestHandoverRequestIsSeenOnce(t *testing.T) {
	fastHandoverPoll(t)
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := watchHandover(ctx, dir)
	if err := requestHandover(dir); err != nil {
		t.Fatal(err)
	}
	select {
	case <-requests:
	case <-time.After(2 * time.Second):
		t.Fatal("the request wasn't seen")
	}
	select {
	case <-requests:
		t.Error("seen twice")
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := os.Stat(handoverPath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the request file was left: %v", err)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("left %v", left)
	}
}

// A request left by an explicit DOGGIE that gave up or died is dropped.
func TestStaleHandoverRequestIsDropped(t *testing.T) {
	fastHandoverPoll(t)
	dir := t.TempDir()
	if err := requestHandover(dir); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * handoverTimeout)
	os.Chtimes(handoverPath(dir), old, old)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := watchHandover(ctx, dir)
	select {
	case <-requests:
		t.Error("handed over for a stale request")
	case <-time.After(200 * time.Millisecond):
	}
	if _, err := os.Stat(handoverPath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the stale request was left: %v", err)
	}
}
