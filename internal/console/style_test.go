package console

import (
	"bytes"
	"strings"
	"testing"
)

func TestDetectProfile(t *testing.T) {
	for _, c := range []struct {
		env  map[string]string
		want Profile
	}{
		{map[string]string{}, ANSI16},
		{map[string]string{"TERM": "xterm"}, ANSI16},
		{map[string]string{"TERM": "xterm-256color"}, ANSI256},
		{map[string]string{"TERM": "xterm-256color", "COLORTERM": "truecolor"}, TrueColor},
		{map[string]string{"COLORTERM": "24BIT"}, TrueColor},
		{map[string]string{"TERM": "xterm-256color", "COLORTERM": "truecolor", "NO_COLOR": "1"}, NoColor},
		{map[string]string{"TERM": "dumb", "COLORTERM": "truecolor"}, NoColor},
		{map[string]string{"NO_COLOR": ""}, ANSI16}, // set but empty is unset
	} {
		if got := DetectProfile(func(k string) string { return c.env[k] }); got != c.want {
			t.Errorf("%v: %v, want %v", c.env, got, c.want)
		}
	}
}

func TestTo256(t *testing.T) {
	for _, c := range []struct {
		c    RGB
		want int
	}{
		{RGB{0, 0, 0}, 16},
		{RGB{255, 0, 0}, 196},
		{RGB{255, 255, 255}, 231},
		{RGB{0x80, 0x80, 0x80}, 244},
		{RGB{0x08, 0x08, 0x08}, 232},
		{RGB{0xe8, 0x87, 0x3a}, 173},
	} {
		if got := to256(c.c); got != c.want {
			t.Errorf("%v: %d, want %d", c.c, got, c.want)
		}
	}
	// Every color maps into the palette's cube or gray ramp.
	for v := 0; v < 256; v += 5 {
		for _, c := range []RGB{{uint8(v), 0, 0}, {uint8(v), uint8(v), uint8(v)}, {0, uint8(v), uint8(255 - v)}} {
			if got := to256(c); got < 16 || got > 255 {
				t.Fatalf("%v: %d", c, got)
			}
		}
	}
}

func TestLookSequences(t *testing.T) {
	l := Look{Color: RGB{1, 2, 3}, ANSI: 33, Bold: true}
	for p, want := range map[Profile]string{
		NoColor:   "",
		ANSI16:    "\x1b[1;33m",
		ANSI256:   "\x1b[1;38;5;16m",
		TrueColor: "\x1b[1;38;2;1;2;3m",
	} {
		if got := l.sgr(p); got != want {
			t.Errorf("profile %d: %q, want %q", p, got, want)
		}
	}
	if got := (Look{}).sgr(TrueColor); got != "" {
		t.Errorf("the terminal's own look: %q", got)
	}
	if got := (Look{ANSI: 90}).sgr(TrueColor); got != "\x1b[90m" {
		t.Errorf("only a 16-color fallback: %q", got)
	}
	if got := (Look{Color: RGB{9, 9, 9}}).sgr(ANSI16); got != "\x1b[39m" {
		t.Errorf("no 16-color fallback: %q", got)
	}
}

// Each run of a role is drawn with its look, and every row ends in the
// terminal's own, so nothing leaks into what comes after.
func TestStyledRows(t *testing.T) {
	s := &screen{cols: 40}
	s.style(nil, ANSI16)
	var b bytes.Buffer
	s.printText(&b, Text{Text: "DOGGIE", Role: Assistant})
	s.printText(&b, Text{Text: "  hola "})
	s.printText(&b, Text{Text: "ok", Role: Good})
	s.printText(&b, Text{Text: "\n"})
	want := "\x1b[1;33mDOGGIE\x1b[0m  hola \x1b[32mok\x1b[0m\r\n"
	if got := b.String(); got != want {
		t.Fatalf("%q, want %q", got, want)
	}

	plain := &screen{cols: 40}
	plain.style(nil, NoColor)
	b.Reset()
	plain.printText(&b, Text{Text: "DOGGIE", Role: Assistant})
	plain.printText(&b, Text{Text: " hola\n", Role: Muted})
	if got := b.String(); got != "DOGGIE hola\r\n" {
		t.Errorf("without color: %q", got)
	}
}

// The light sweeps across the label, and needs at least 256 colors.
func TestShimmer(t *testing.T) {
	label := []rune("thinking")
	s := &screen{cols: 40}
	s.style(nil, TrueColor)
	frames := map[string]bool{}
	for s.phase = 0; s.phase < len(label)+2*shimmerWidth; s.phase++ {
		var b bytes.Buffer
		s.shimmer(&b, label)
		if got := stripSGR(b.String()); got != "thinking" {
			t.Fatalf("phase %d shows %q", s.phase, got)
		}
		frames[b.String()] = true
	}
	if len(frames) < len(label) {
		t.Errorf("only %d distinct frames", len(frames))
	}
	s.style(nil, ANSI16)
	var b bytes.Buffer
	s.shimmer(&b, label)
	if got := b.String(); got != "\x1b[90mthinking\x1b[0m" {
		t.Errorf("16 colors: %q", got)
	}
}

// stripSGR drops style sequences.
func stripSGR(s string) string {
	var out strings.Builder
	for len(s) > 0 {
		if strings.HasPrefix(s, "\x1b[") {
			if i := strings.IndexByte(s, 'm'); i >= 0 {
				s = s[i+1:]
				continue
			}
		}
		out.WriteByte(s[0])
		s = s[1:]
	}
	return out.String()
}
