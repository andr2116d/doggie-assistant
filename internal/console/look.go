package console

// palette is the full screen's colors. Backgrounds keep the terminal's own
// in 16 colors, where painting them would clash with its theme.
type palette struct {
	bg, chrome, line   Color // grounds and rules
	fg, muted          Color // text
	accent, accentLine Color // the assistant's color, and its borders
	you, youLine       Color // the ground of what the user said, and its edge
	good, wait, bad    Color // how things went
}

// violetMint is the default palette: violet for the assistant, mint for
// what went well, amber for what waits, on a dark violet ground.
var violetMint = palette{
	bg:         Hex(0x100e16, NoANSI),
	chrome:     Hex(0x18151f, NoANSI),
	line:       Hex(0x2a2436, 8),
	fg:         Hex(0xece9f4, NoANSI),
	muted:      Hex(0x948da8, 8),
	accent:     Hex(0xa78bfa, 13),
	accentLine: Hex(0x55468a, 5),
	you:        Hex(0x201b2c, 8),
	youLine:    Hex(0x3a3150, 8),
	good:       Hex(0x6ee7b7, 10),
	wait:       Hex(0xf4c95d, 11),
	bad:        Hex(0xf87171, 9),
}

// looks are the styles each part of the full screen is drawn with.
type looks struct {
	base, text, muted          styleID // the ground, text on it, notes
	header, brand, headerMuted styleID // the top bar
	online, offline            styleID // the connection, on the bar
	box, prompt, input, hint   styleID // the input box and the hints below
	you, youEdge               styleID // a bubble of what the user said
	name, border               styleID // a bubble of the assistant
	good, wait, bad            styleID // how things went
	pill                       styleID // "new messages below"
	line, cardTitle            styleID // rules and the panel's cards
	selected                   styleID // text marked with the mouse
	// Markdown in a reply.
	bold, italic, code, heading styleID
}

func newLooks(t *styleTable, p palette) looks {
	on := func(fg, bg Color, bold bool) styleID { return t.id(Style{Fg: fg, Bg: bg, Bold: bold}) }
	return looks{
		base:        on(Color{}, p.bg, false),
		text:        on(p.fg, p.bg, false),
		muted:       on(p.muted, p.bg, false),
		header:      on(p.fg, p.chrome, false),
		brand:       on(p.accent, p.chrome, true),
		headerMuted: on(p.muted, p.chrome, false),
		online:      on(p.good, p.chrome, false),
		offline:     on(p.wait, p.chrome, false),
		box:         on(p.accentLine, p.bg, false),
		prompt:      on(p.accent, p.bg, true),
		input:       on(p.fg, p.bg, false),
		hint:        on(p.muted, p.bg, false),
		you:         on(p.fg, p.you, false),
		youEdge:     on(p.youLine, p.bg, false),
		name:        on(p.accent, p.bg, true),
		border:      on(p.accentLine, p.bg, false),
		good:        on(p.good, p.bg, false),
		wait:        on(p.wait, p.bg, false),
		bad:         on(p.bad, p.bg, false),
		pill:        on(p.accent, p.you, true),
		line:        on(p.line, p.bg, false),
		cardTitle:   on(p.muted, p.bg, true),
		selected:    on(p.fg, p.accentLine, false),
		bold:        on(p.fg, p.bg, true),
		italic:      t.id(Style{Fg: p.fg, Bg: p.bg, Italic: true}),
		code:        on(p.good, p.you, false),
		heading:     on(p.accent, p.bg, true),
	}
}

// forAttr is the look of text with markdown attribute a: code first, then
// a heading, then bold, then italic.
func (lk looks) forAttr(a mdAttr) styleID {
	switch {
	case a&mdCode != 0:
		return lk.code
	case a&mdHeading != 0:
		return lk.heading
	case a&mdBold != 0:
		return lk.bold
	case a&mdItalic != 0:
		return lk.italic
	}
	return lk.text
}
