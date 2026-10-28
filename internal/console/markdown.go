package console

import "strings"

// mdAttr is how markdown marks a run of text; attributes add up.
type mdAttr uint8

const (
	mdBold mdAttr = 1 << iota
	mdItalic
	mdCode
	mdHeading
)

// mdPara is one paragraph of markdown as it shows: its text without the
// marks, the attribute of each byte of it, and how far the rows it wraps
// into are indented, as under a list item's text.
type mdPara struct {
	text  string
	attrs []mdAttr
	hang  int
	// fence is set for a line that opens or closes a block of code: it
	// doesn't show, and turns code on or off for the paragraphs after.
	fence bool
}

// markdown renders one paragraph, which has no newline. In a block of code
// (inCode) it shows as it is. Marks open as soon as they come and close at
// their pair or at the paragraph's end, so text written a piece at a time
// never changes once shown: what follows an opening mark is marked at once.
// Only what can be told apart while text comes in is understood: **bold**,
// *italic*, `code`, # headings, and - or 1. lists.
func markdown(para string, inCode bool) mdPara {
	if strings.HasPrefix(strings.TrimLeft(para, " "), "```") {
		return mdPara{fence: true}
	}
	if inCode {
		return mdPara{text: para, attrs: fill(len(para), mdCode)}
	}
	var p mdPara
	var b strings.Builder
	var attrs []mdAttr
	add := func(s string, a mdAttr) {
		b.WriteString(s)
		for range len(s) {
			attrs = append(attrs, a)
		}
	}
	rest := para
	indent := len(rest) - len(strings.TrimLeft(rest, " "))
	body := rest[indent:]
	var base mdAttr
	switch {
	case headingLevel(body) > 0:
		n := headingLevel(body)
		rest, base = body[n+1:], mdHeading|mdBold
	case strings.HasPrefix(body, "- ") || strings.HasPrefix(body, "* ") || strings.HasPrefix(body, "+ "):
		add(strings.Repeat(" ", indent)+"• ", 0)
		rest, p.hang = body[2:], indent+2
	case orderedItem(body) > 0:
		n := orderedItem(body)
		add(strings.Repeat(" ", indent)+body[:n], 0)
		rest, p.hang = body[n:], indent+n
	}
	var on mdAttr
	for i := 0; i < len(rest); {
		c := rest[i]
		switch {
		case on&mdCode != 0 && c == '`':
			on &^= mdCode
			i++
		case on&mdCode != 0:
			j := strings.IndexByte(rest[i:], '`')
			if j < 0 {
				j = len(rest) - i
			}
			add(rest[i:i+j], base|on)
			i += j
		case c == '\\' && i+1 < len(rest) && strings.IndexByte("*`\\#_", rest[i+1]) >= 0:
			add(rest[i+1:i+2], base|on)
			i += 2
		case c == '`':
			on |= mdCode
			i++
		case strings.HasPrefix(rest[i:], "**"):
			on ^= mdBold
			i += 2
		case c == '*' && emphasis(rest, i, on&mdItalic != 0):
			on ^= mdItalic
			i++
		default:
			j := i + 1
			for j < len(rest) && strings.IndexByte("`*\\", rest[j]) < 0 {
				j++
			}
			add(rest[i:j], base|on)
			i = j
		}
	}
	p.text, p.attrs = b.String(), attrs
	return p
}

// emphasis reports whether the * at i opens italic text, being followed by
// something other than a space, or closes it, being after such a thing, so
// "2 * 3" stays as it is. A * that ends the text so far opens it.
func emphasis(s string, i int, open bool) bool {
	if open {
		return i > 0 && s[i-1] != ' '
	}
	return i+1 == len(s) || s[i+1] != ' '
}

// headingLevel is how many # start a heading line, 0 if it isn't one.
func headingLevel(s string) int {
	n := 0
	for n < len(s) && n < 6 && s[n] == '#' {
		n++
	}
	if n == 0 || n >= len(s) || s[n] != ' ' {
		return 0
	}
	return n
}

// orderedItem is how long the "12. " starting a numbered list item is, 0
// if it isn't one.
func orderedItem(s string) int {
	n := 0
	for n < len(s) && n < 4 && s[n] >= '0' && s[n] <= '9' {
		n++
	}
	if n == 0 || n+1 >= len(s) || s[n] != '.' || s[n+1] != ' ' {
		return 0
	}
	return n + 2
}

func fill(n int, a mdAttr) []mdAttr {
	attrs := make([]mdAttr, n)
	for i := range attrs {
		attrs[i] = a
	}
	return attrs
}
