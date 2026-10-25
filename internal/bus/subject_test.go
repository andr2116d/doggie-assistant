package bus

import (
	"testing"
)

func TestNewSubjectValidation(t *testing.T) {
	for _, typ := range []string{"task.done", "system.heartbeat", "a"} {
		NewSubject[ping](typ) // must not panic
	}
	for _, typ := range []string{"", "task.", ".task", "task..done", "task.*", "task.>", "task done"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("NewSubject(%q) did not panic", typ)
				}
			}()
			NewSubject[ping](typ)
		}()
	}
}
