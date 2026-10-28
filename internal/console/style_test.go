package console

import "testing"

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
