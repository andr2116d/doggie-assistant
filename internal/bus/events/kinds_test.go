package events

import (
	"testing"
	"time"
)

func TestTimeoutOf(t *testing.T) {
	if got := TimeoutOf("timer"); got != 24*time.Hour || got <= MaxTimer {
		t.Errorf("a timer may take %s, want 24h, more than the %s it may count", got, MaxTimer)
	}
	for _, kind := range []string{"demo.sleep", "unknown.kind", ""} {
		if got := TimeoutOf(kind); got != 0 {
			t.Errorf("%q may take %s, want the manager's default", kind, got)
		}
	}
}

func TestPersonMustAnswer(t *testing.T) {
	kinds := []TaskKind{{Name: "demo.ask", Risk: RiskAct}, {Name: "lock.open", Risk: RiskSensitive}}
	for _, tt := range []struct {
		kind      string
		sensitive bool
		want      bool
	}{
		{"demo.ask", false, false},
		{"demo.ask", true, true},
		{"lock.open", false, true},
		{"unknown.kind", false, true},
		{"", false, true},
	} {
		info := TaskInfo{TaskSpec: TaskSpec{Kind: tt.kind, Sensitive: tt.sensitive}}
		if got := PersonMustAnswer(info, kinds); got != tt.want {
			t.Errorf("PersonMustAnswer(%s, sensitive %v) = %v, want %v", tt.kind, tt.sensitive, got, tt.want)
		}
	}
	for _, k := range TaskKinds() {
		if PersonMustAnswer(TaskInfo{TaskSpec: TaskSpec{Kind: k.Name}}, TaskKinds()) != (k.Risk == RiskSensitive) {
			t.Errorf("%s: wrong answer for its risk %s", k.Name, k.Risk)
		}
	}
}
