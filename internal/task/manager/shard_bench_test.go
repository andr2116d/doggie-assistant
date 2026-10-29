package manager

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus/events"
	"github.com/andr2116d/doggie-assistant/internal/task"
)

// benchShard is a shard holding n tasks, as a day of retention does: the
// last two run and beat, the others are over.
func benchShard(b *testing.B, n int) *shard {
	b.Helper()
	m := &Manager{log: slog.New(slog.NewTextHandler(io.Discard, nil)), opts: Options{HeartbeatInterval: 5 * time.Second}}
	s := newShard(m)
	now := time.Now()
	for i := range n {
		id := "t" + strconv.Itoa(i+1)
		t := task.New(id, events.TaskSpec{Kind: "demo.sleep", Description: "a task of the day"}, now, now.Add(time.Hour))
		if i < n-2 {
			t.Fail("over", now)
		} else {
			s.lastBeat[id] = now
		}
		s.tasks[id] = t
	}
	return s
}

// The liveness check runs every heartbeat interval, whatever the shard
// holds.
func BenchmarkCheckLiveness(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			s := benchShard(b, n)
			b.ReportAllocs()
			for b.Loop() {
				s.checkLiveness(context.Background())
			}
		})
	}
}
