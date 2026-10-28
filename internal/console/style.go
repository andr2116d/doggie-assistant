package console

import (
	"fmt"
	"strings"
)

// Role is what a piece of text is to the reader. The theme gives each role
// its look, so callers never pick colors.
type Role uint8

const (
	// Normal text has the terminal's own look.
	Normal Role = iota
	// Assistant is the assistant's name before what it says.
	Assistant
	// User is what the user said.
	User
	// Muted are notes about the chat itself.
	Muted
	// Good is something that went well, e.g. a finished task.
	Good
	// Warn is something that needs the user, e.g. a question.
	Warn
	// Bad is something that went wrong.
	Bad
	// Accent marks the prompt and the activity indicator.
	Accent
	numRoles
)

// Profile is how much color a terminal shows.
type Profile uint8

const (
	// NoColor prints text alone, with no styling at all.
	NoColor Profile = iota
	// ANSI16 uses the terminal's own 16 colors, so its palette is kept.
	ANSI16
	// ANSI256 uses the 256-color palette.
	ANSI256
	// TrueColor uses 24-bit color.
	TrueColor
)

// DetectProfile tells a terminal's colors from its environment, read with
// getenv: NO_COLOR (any value) or TERM=dumb turn styling off, COLORTERM
// truecolor or 24bit gives 24-bit color, and a TERM naming 256color gives
// 256 colors.
func DetectProfile(getenv func(string) string) Profile {
	term := getenv("TERM")
	switch {
	case getenv("NO_COLOR") != "", term == "dumb":
		return NoColor
	}
	switch strings.ToLower(getenv("COLORTERM")) {
	case "truecolor", "24bit":
		return TrueColor
	}
	if strings.Contains(term, "256color") {
		return ANSI256
	}
	return ANSI16
}

// RGB is a 24-bit color.
type RGB struct{ R, G, B uint8 }

// Look is how a role is drawn.
type Look struct {
	// Color is the 24-bit color, also the base of the 256-color one. Zero
	// with ANSI zero means the terminal's own color.
	Color RGB
	// ANSI is the foreground in 16 colors: 30-37 or 90-97, 0 for the
	// terminal's own.
	ANSI              int
	Bold, Dim, Italic bool
}

// Theme is the look of each role.
type Theme [numRoles]Look

// DefaultTheme reads on dark and light backgrounds alike. DOGGIE's warm
// orange marks what is its own: its name, the prompt and its activity.
var DefaultTheme = Theme{
	Normal:    {},
	Assistant: {Color: RGB{0xe8, 0x87, 0x3a}, ANSI: 33, Bold: true},
	User:      {Bold: true},
	Muted:     {Color: RGB{0x8a, 0x8f, 0x98}, ANSI: 90},
	Good:      {Color: RGB{0x5f, 0xb8, 0x78}, ANSI: 32},
	Warn:      {Color: RGB{0xe5, 0xb7, 0x50}, ANSI: 33},
	Bad:       {Color: RGB{0xe0, 0x6c, 0x75}, ANSI: 31},
	Accent:    {Color: RGB{0xe8, 0x87, 0x3a}, ANSI: 33},
}

// colored reports whether l has a color of its own.
func (l Look) colored() bool { return l.ANSI != 0 || l.Color != (RGB{}) }

// sgr is the sequence that draws l on profile p, starting from the terminal's
// own look; empty when there is nothing to change.
func (l Look) sgr(p Profile) string {
	if p == NoColor {
		return ""
	}
	var codes []string
	if l.Bold {
		codes = append(codes, "1")
	}
	if l.Dim {
		codes = append(codes, "2")
	}
	if l.Italic {
		codes = append(codes, "3")
	}
	if l.colored() {
		codes = append(codes, colorCode(l.Color, l.ANSI, p))
	}
	if len(codes) == 0 {
		return ""
	}
	return "\x1b[" + strings.Join(codes, ";") + "m"
}

// colorCode is the foreground parameter for c on profile p, falling back to
// ansi in 16 colors.
func colorCode(c RGB, ansi int, p Profile) string {
	switch {
	case p == TrueColor && c != (RGB{}):
		return fmt.Sprintf("38;2;%d;%d;%d", c.R, c.G, c.B)
	case p == ANSI256 && c != (RGB{}):
		return fmt.Sprintf("38;5;%d", to256(c))
	case ansi != 0:
		return fmt.Sprint(ansi)
	default:
		return "39"
	}
}

// to256 is the nearest color of the 256-color palette's 6×6×6 cube or its
// gray ramp.
func to256(c RGB) int {
	level := func(v uint8) int {
		if v < 48 {
			return 0
		}
		if v < 115 {
			return 1
		}
		return (int(v) - 35) / 40
	}
	steps := [6]int{0, 95, 135, 175, 215, 255}
	r, g, b := level(c.R), level(c.G), level(c.B)
	cube := 16 + 36*r + 6*g + b
	gray := (int(c.R) + int(c.G) + int(c.B)) / 3
	grayIdx := min(max(gray-3, 0)/10, 23) // nearest of 8, 18, ... 238
	grayLevel := 8 + 10*grayIdx
	dist := func(r, g, b int) int {
		dr, dg, db := int(c.R)-r, int(c.G)-g, int(c.B)-b
		return dr*dr + dg*dg + db*db
	}
	if dist(grayLevel, grayLevel, grayLevel) < dist(steps[r], steps[g], steps[b]) {
		return 232 + grayIdx
	}
	return cube
}

// blend is the color t of the way from a to b, t in 0-1.
func blend(a, b RGB, t float64) RGB {
	mix := func(x, y uint8) uint8 { return uint8(float64(x) + (float64(y)-float64(x))*t + 0.5) }
	return RGB{mix(a.R, b.R), mix(a.G, b.G), mix(a.B, b.B)}
}

// reset is the sequence back to the terminal's own look.
const reset = "\x1b[0m"
