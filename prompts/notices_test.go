package prompts

import (
	"testing"
	"time"
)

func TestUsageNotices(t *testing.T) {
	lima := time.FixedZone("Lima", -5*3600)
	now := time.Date(2026, 10, 25, 18, 51, 0, 0, lima) // a Sunday
	for _, tc := range []struct {
		name, lang string
		renews     time.Time
		low        bool
		want       string
	}{
		{"today", "es", time.Date(2026, 10, 25, 21, 10, 0, 0, lima), false,
			"Ahora no puedo pensar: se me acabó la cuota y se renueva a las 21:10. Las tareas en curso siguen."},
		{"region", "es-PE", time.Date(2026, 10, 25, 21, 10, 0, 0, lima), true,
			"Ojo: me queda poca cuota para pensar. Se renueva a las 21:10."},
		{"another zone", "es", time.Date(2026, 10, 26, 2, 10, 0, 0, time.UTC), true,
			"Ojo: me queda poca cuota para pensar. Se renueva a las 21:10."},
		{"one o'clock", "es", time.Date(2026, 10, 26, 1, 5, 0, 0, lima), true,
			"Ojo: me queda poca cuota para pensar. Se renueva mañana a la 1:05."},
		{"this week", "es", time.Date(2026, 10, 28, 9, 0, 0, 0, lima), false,
			"Ahora no puedo pensar: se me acabó la cuota y se renueva el miércoles a las 9:00. Las tareas en curso siguen."},
		{"later", "es", time.Date(2026, 11, 3, 9, 0, 0, 0, lima), true,
			"Ojo: me queda poca cuota para pensar. Se renueva el 3/11 a las 9:00."},
		{"unknown", "es", time.Time{}, false,
			"Ahora no puedo pensar: se me acabó la cuota por un rato. Las tareas en curso siguen."},
		{"past", "es", now.Add(-time.Minute), true, "Ojo: me queda poca cuota para pensar."},
		{"english tomorrow", "en", time.Date(2026, 10, 26, 0, 30, 0, 0, lima), false,
			"I can't think right now: I've run out of quota, and it renews tomorrow at 0:30. Tasks already running carry on."},
		{"english this week", "en", time.Date(2026, 10, 31, 21, 10, 0, 0, lima), true,
			"Heads up: I'm running low on quota. It renews on Saturday at 21:10."},
		{"english later", "en", time.Date(2026, 11, 2, 21, 10, 0, 0, lima), true,
			"Heads up: I'm running low on quota. It renews on Nov 2 at 21:10."},
		{"other language", "qu", time.Time{}, true, "Heads up: I'm running low on quota."},
		{"blank language", " ", time.Date(2026, 10, 25, 21, 10, 0, 0, lima), true,
			"Heads up: I'm running low on quota. It renews at 21:10."},
	} {
		got := UsageSpent(tc.lang, tc.renews, now)
		if tc.low {
			got = UsageLow(tc.lang, tc.renews, now)
		}
		if got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

// Days are counted on the calendar, even across a change of daylight time.
func TestCalendarDays(t *testing.T) {
	madrid, err := time.LoadLocation("Europe/Madrid")
	if err != nil {
		t.Skip(err)
	}
	now := time.Date(2026, 10, 24, 23, 30, 0, 0, madrid) // the night before the clocks go back
	for renews, want := range map[time.Time]int{
		time.Date(2026, 10, 24, 23, 59, 0, 0, madrid): 0,
		time.Date(2026, 10, 25, 0, 1, 0, 0, madrid):   1,
		time.Date(2026, 10, 25, 23, 59, 0, 0, madrid): 1,
		time.Date(2026, 10, 26, 0, 0, 0, 0, madrid):   2,
		time.Date(2026, 10, 31, 12, 0, 0, 0, madrid):  7,
	} {
		if got := calendarDays(now, renews); got != want {
			t.Errorf("calendarDays to %s = %d, want %d", renews, got, want)
		}
	}
}
