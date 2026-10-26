package prompts

import (
	"fmt"
	"strings"
	"time"
)

// Notices are what DOGGIE says when its language model can't write the words
// itself, e.g. once its usage limit is spent. They come in a few languages;
// any other gets English.

// notice is one notice in one language: renews has a %s for when the limit
// renews, and unknown is said when that isn't known.
type notice struct{ renews, unknown string }

var spent = map[string]notice{
	"en": {"I can't think right now: I've run out of quota, and it renews %s. Tasks already running carry on.",
		"I can't think right now: I've run out of quota for a while. Tasks already running carry on."},
	"es": {"Ahora no puedo pensar: se me acabó la cuota y se renueva %s. Las tareas en curso siguen.",
		"Ahora no puedo pensar: se me acabó la cuota por un rato. Las tareas en curso siguen."},
}

var low = map[string]notice{
	"en": {"Heads up: I'm running low on quota. It renews %s.", "Heads up: I'm running low on quota."},
	"es": {"Ojo: me queda poca cuota para pensar. Se renueva %s.", "Ojo: me queda poca cuota para pensar."},
}

// UsageSpent is what DOGGIE says, in the language lang (BCP 47), when its
// usage limit is spent until renews (zero if unknown). The time is told in
// now's time zone, which should be the owner's.
func UsageSpent(lang string, renews, now time.Time) string {
	return say(spent, lang, renews, now)
}

// UsageLow is what DOGGIE says when little of its usage limit is left, like
// UsageSpent.
func UsageLow(lang string, renews, now time.Time) string {
	return say(low, lang, renews, now)
}

func say(texts map[string]notice, lang string, renews, now time.Time) string {
	base, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(lang)), "-")
	if _, ok := texts[base]; !ok {
		base = "en"
	}
	n := texts[base]
	if renews.IsZero() || !renews.After(now) {
		return n.unknown
	}
	return fmt.Sprintf(n.renews, when(base, renews.In(now.Location()), now))
}

var weekdays = map[string][7]string{
	"en": {"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"},
	"es": {"domingo", "lunes", "martes", "miércoles", "jueves", "viernes", "sábado"},
}

// when tells a time in the next days, from now: "at 21:10", "tomorrow at
// 9:00", "on Tuesday at 21:10", or a date further on.
func when(lang string, t, now time.Time) string {
	clock := fmt.Sprintf("%d:%02d", t.Hour(), t.Minute())
	days := calendarDays(now, t)
	at := "a las " + clock
	if t.Hour() == 1 {
		at = "a la " + clock // "la una"
	}
	switch {
	case lang == "es" && days == 0:
		return at
	case lang == "es" && days == 1:
		return "mañana " + at
	case lang == "es" && days < 7:
		return "el " + weekdays["es"][t.Weekday()] + " " + at
	case lang == "es":
		return "el " + t.Format("2/1") + " " + at
	case days == 0:
		return "at " + clock
	case days == 1:
		return "tomorrow at " + clock
	case days < 7:
		return "on " + weekdays["en"][t.Weekday()] + " at " + clock
	}
	return "on " + t.Format("Jan 2") + " at " + clock
}

// calendarDays is how many midnights there are from a to b, in a's time zone.
func calendarDays(a, b time.Time) int {
	b = b.In(a.Location())
	day := func(t time.Time) time.Time {
		y, m, d := t.Date()
		return time.Date(y, m, d, 12, 0, 0, 0, time.UTC)
	}
	return int(day(b).Sub(day(a)).Hours() / 24)
}
