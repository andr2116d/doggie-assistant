package console

import (
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPacerTakesInOrder(t *testing.T) {
	var p pacer
	p.add(Text{Text: "héllo", Role: Assistant, Gap: true, Indent: 2})
	p.add(Text{Text: ""})
	p.add(Text{Text: "日本", Wrap: true})
	var got []Text
	for !p.empty() {
		got = append(got, p.take(2)...)
	}
	want := []Text{
		{Text: "hé", Role: Assistant, Gap: true, Indent: 2},
		{Text: "ll", Role: Assistant, Indent: 2},
		{Text: "o", Role: Assistant, Indent: 2},
		{Text: ""},
		{Text: "日", Wrap: true},
		{Text: "本", Wrap: true},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("\n%+v\nwant\n%+v", got, want)
	}
	if p.runes != 0 {
		t.Errorf("%d runes left", p.runes)
	}
}

// The pace grows with what waits: a burst shows in about catchUp, a big
// backlog shows at once, and a frame never shows less than minStep.
func TestPacerStep(t *testing.T) {
	var p pacer
	p.add(Text{Text: strings.Repeat("a", 250)})
	frames := 0
	for !p.empty() {
		n := 0
		for _, x := range p.step() {
			n += utf8.RuneCountInString(x.Text)
		}
		if n < minStep && !p.empty() {
			t.Fatalf("frame %d showed %d runes", frames, n)
		}
		frames++
	}
	// About 250 ms of 16 ms frames to show most of it, then the tail.
	if frames < 10 || frames > 60 {
		t.Errorf("a 250-rune burst took %d frames", frames)
	}

	p.add(Text{Text: strings.Repeat("b", maxBacklog+1)})
	if got := p.step(); len(got) != 1 || !p.empty() {
		t.Errorf("a big backlog didn't show at once: %d pieces left", len(p.queue))
	}
}

func TestPacerInvalidUTF8(t *testing.T) {
	var p pacer
	p.add(Text{Text: "a\xffb"})
	var got string
	for !p.empty() {
		for _, x := range p.take(1) {
			got += x.Text
		}
	}
	if got != "a\xffb" || p.runes != 0 {
		t.Errorf("%q, %d runes left", got, p.runes)
	}
}
