package console

import "strings"

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
