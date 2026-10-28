package console

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func newer(n int) string { return fmt.Sprintf("↓ %d nuevos", n) }

// sampleConversation is a short chat with every kind of message.
func sampleConversation() *conversation {
	c := newConversation("DOGGIE")
	c.say(fromUser, "", "Recuérdame en 10 minutos sacar la ropa de la lavadora y cuéntame un chiste corto", noteInfo)
	c.finish("r1", "Listo, te aviso en 10 minutos.")
	c.noteOn("r1", notice{noteGood, "temporizador «ropa» · 10:00"})
	c.finish("r2", "Ahí va: ¿por qué el libro de matemáticas estaba triste? Porque tenía demasiados problemas. 📚")
	c.say(fromElsewhere, "voice:kitchen", "¿y mañana a qué hora es mi reunión?", noteInfo)
	c.say(fromChat, "", "lost the connection to DOGGIE; reconnecting…", noteWait)
	return c
}

// drawConversation draws c on a w×h screen with the chrome and returns
// the screen's text.
func drawConversation(c *conversation, w, h int) (string, *grid) {
	var g grid
	g.resize(w, h)
	lk := newLooks(newStyleTable(), violetMint)
	r := layout(w, h, true, 1)
	drawChrome(&g, r, lk, sampleChrome)
	c.draw(&g, r.body, lk, newer)
	return screenText(&g), &g
}

func TestConversationGolden(t *testing.T) {
	for _, size := range [][2]int{{60, 26}, {80, 26}, {120, 30}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			text, _ := drawConversation(sampleConversation(), size[0], size[1])
			golden(t, fmt.Sprintf("convo-%dx%d.txt", size[0], size[1]), text)
		})
	}
}

// A reply that comes in pieces wraps the same as the whole of it.
func TestStreamedReplyWrapsAsWhole(t *testing.T) {
	text := "Claro: la reunión es el jueves a las nueve.\nAnoté que lleves los papeles del seguro y el recibo de la luz, y te aviso una hora antes."
	for _, n := range []int{1, 3, 7, 20} {
		c := newConversation("DOGGIE")
		for s := text; s != ""; {
			k := min(n, len(s))
			for k < len(s) && !utf8RuneStart(s[k]) {
				k++
			}
			c.stream("r", s[:k])
			s = s[k:]
			for c.pace() {
				c.msgs[0].lines(30) // as frames do
			}
			c.msgs[0].lines(30)
		}
		if got, want := c.msgs[0].lines(30), wrapLines(text, 30); !slices.Equal(got, want) {
			t.Fatalf("in pieces of %d:\n%q\nwant\n%q", n, got, want)
		}
	}
}

func utf8RuneStart(b byte) bool { return b&0xc0 != 0x80 }

// A reply's text is paced in: it shows a little at a time, all in the end.
func TestReplyIsPacedIn(t *testing.T) {
	c := newConversation("DOGGIE")
	c.stream("r", strings.Repeat("palabra ", 40))
	m := c.msgs[0]
	if m.shown != 0 {
		t.Fatalf("%d bytes shown before any frame", m.shown)
	}
	frames := 0
	for c.pace() {
		frames++
		if m.shown == 0 || m.shown == len(m.text) {
			t.Fatalf("frame %d shows %d of %d", frames, m.shown, len(m.text))
		}
	}
	if m.shown != len(m.text) || frames < 5 {
		t.Errorf("%d frames, %d of %d shown", frames, m.shown, len(m.text))
	}
	// The whole block corrects the pieces; one never streamed shows at once.
	c.finish("r", "otra cosa")
	if m.text != "otra cosa" || m.shown > len("otra cosa") {
		t.Errorf("corrected: %q, %d", m.text, m.shown)
	}
	c.finish("r2", "de una vez")
	if last := c.msgs[1]; last.shown != len(last.text) {
		t.Error("a reply never streamed was paced")
	}
}

// Scrolled up, the view stays on what it shows while new messages come,
// old ones go and the terminal narrows; back at the bottom it follows.
func TestScrollStaysPut(t *testing.T) {
	c := newConversation("DOGGIE")
	for i := range 30 {
		c.say(fromUser, "", fmt.Sprintf("mensaje número %d", i), noteInfo)
	}
	before, _ := drawConversation(c, 80, 24)
	c.scrollBy(10)
	up, _ := drawConversation(c, 80, 24)
	if up == before {
		t.Fatal("scrolling changed nothing")
	}
	c.say(fromUser, "", "uno nuevo", noteInfo)
	c.stream("r", "y una respuesta que va llegando")
	for c.pace() {
	}
	after, _ := drawConversation(c, 80, 24)
	if strip(after) != strip(up) || !strings.Contains(after, "↓ 2 nuevos") {
		t.Errorf("the view moved:\n%s\nwas:\n%s", after, up)
	}
	c.scrollBy(-1000)
	bottom, _ := drawConversation(c, 80, 24)
	if !strings.Contains(bottom, "y una respuesta que va llegando") || strings.Contains(bottom, "nuevos") {
		t.Errorf("back at the bottom:\n%s", bottom)
	}
}

// strip drops the last rows of the conversation, where the pill goes, and
// the chrome around it.
func strip(screen string) string {
	rows := strings.Split(screen, "\n")
	return strings.Join(rows[1:len(rows)-6], "\n")
}

func TestScrollStopsAtTheTop(t *testing.T) {
	c := sampleConversation()
	c.scrollBy(1000)
	text, _ := drawConversation(c, 80, 26)
	if !c.top || !strings.Contains(text, "Recuérdame") {
		t.Errorf("not at the top (%v):\n%s", c.top, text)
	}
	if c.scroll > 1000 || c.scroll < 0 {
		t.Errorf("scroll %d", c.scroll)
	}
}

func TestConversationKeepsAtMost500(t *testing.T) {
	c := newConversation("DOGGIE")
	for i := range maxMessages + 20 {
		c.finish(fmt.Sprint(i), "hola")
	}
	if len(c.msgs) != maxMessages || c.byID["0"] != nil || c.byID["519"] == nil {
		t.Errorf("%d kept", len(c.msgs))
	}
	c.older([]*message{{from: fromUser, text: "viejo"}})
	if len(c.msgs) != maxMessages {
		t.Errorf("older went past the bound: %d", len(c.msgs))
	}
	d := newConversation("DOGGIE")
	d.say(fromUser, "", "reciente", noteInfo)
	d.older([]*message{{from: fromUser, text: "viejo"}})
	if d.msgs[0].text != "viejo" || d.msgs[0].shown != len("viejo") {
		t.Errorf("older: %+v", d.msgs[0])
	}
}

// A notice for a message that isn't there shows on its own.
func TestNoticeWithoutMessage(t *testing.T) {
	c := newConversation("DOGGIE")
	c.noteOn("nope", notice{noteBad, "t3 failed"})
	if len(c.msgs) != 1 || c.msgs[0].from != fromChat || c.msgs[0].kind != noteBad {
		t.Errorf("%+v", c.msgs)
	}
}

// A frame of a long conversation while a reply comes in.
func BenchmarkConversationFrame(b *testing.B) {
	c := newConversation("DOGGIE")
	for i := range maxMessages - 1 {
		if i%2 == 0 {
			c.say(fromUser, "", fmt.Sprintf("pregunta %d sobre algo cotidiano, como el clima o la agenda", i), noteInfo)
		} else {
			c.finish(fmt.Sprint(i), "Una respuesta de un par de líneas, con algo de detalle para que se corte en varias filas de la burbuja.")
		}
	}
	styles := newStyleTable()
	lk := newLooks(styles, violetMint)
	var g grid
	g.resize(200, 60)
	r := layout(200, 60, true, 1)
	p := newPainter(styles, TrueColor)
	drawChrome(&g, r, lk, sampleChrome)
	c.draw(&g, r.body, lk, newer)
	p.frame(&g, 0, 0, false)
	b.ReportAllocs()
	n, i := 0, 0
	for b.Loop() {
		i++
		c.stream(fmt.Sprint("live", i/250), "palabra ") // a reply of about 2 KB, then another
		c.pace()
		g.fill(r.body.x, r.body.y, r.body.w, r.body.h, lk.base)
		c.draw(&g, r.body, lk, newer)
		n = len(p.frame(&g, 0, 0, false))
	}
	b.ReportMetric(float64(n), "bytes/frame")
}

// However a reply comes in and is paced, its lines are those of its whole
// text at that point.
func FuzzIncrementalWrap(f *testing.F) {
	f.Add("Claro: la reunión es el jueves.\nAnoté los papeles del seguro.", uint8(12), []byte{3, 1, 9, 2})
	f.Add("ab supercalifragilistico cd  ef\n\n  - lista 日本語 👍🏽", uint8(5), []byte{1, 1, 1})
	f.Add("abcd  efgh  ij", uint8(2), []byte{}) // two spaces where a line breaks
	f.Fuzz(func(t *testing.T, text string, width uint8, cuts []byte) {
		w := 2 + int(width)%40
		text = sanitize(text, true)
		m := &message{from: fromAssistant}
		for s, i := text, 0; s != ""; i++ {
			n := 1
			if i < len(cuts) {
				n = 1 + int(cuts[i])%9
			}
			k := min(n, len(s))
			for k < len(s) && !utf8RuneStart(s[k]) {
				k++
			}
			m.setText(m.text+s[:k], true)
			s = s[k:]
			for m.shown < len(m.text) {
				m.shown += min(1+int(width)%3, len(m.text)-m.shown)
				for m.shown < len(m.text) && !utf8RuneStart(m.text[m.shown]) {
					m.shown++
				}
				if got, want := m.lines(w), wrapLines(m.text[:m.shown], w); !slices.Equal(got, want) {
					t.Fatalf("%q at %d:\n%q\nwant\n%q", m.text[:m.shown], w, got, want)
				}
			}
		}
	})
}

// Text set without its buffer still grows from where it is.
func TestAppendAfterPlainText(t *testing.T) {
	m := &message{text: "hola"}
	m.appendText(" mundo", false)
	if m.text != "hola mundo" || m.shown != len(m.text) {
		t.Errorf("%q %d", m.text, m.shown)
	}
}

// Two spaces where a line breaks: the line after starts with neither, also
// when the reply comes a letter at a time.
func TestStreamedBreakAtTwoSpaces(t *testing.T) {
	text := "abcd  efgh  ij"
	m := &message{from: fromAssistant}
	for _, r := range text {
		m.appendText(string(r), false)
		if got, want := m.lines(4), wrapLines(m.text, 4); !slices.Equal(got, want) {
			t.Fatalf("%q: %q, want %q", m.text, got, want)
		}
	}
}
