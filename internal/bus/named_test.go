package bus

import (
	"context"
	"strings"
	"testing"
	"time"
)

var testUtterance = NewSubject[ping]("nametest.say")

// startNamed runs a bus with a named satellite "voice-room" that may only
// publish testUtterance under "voice:room".
func startNamed(t *testing.T, extra ...Satellite) (*Server, string) {
	t.Helper()
	const token = "voice-room-token"
	hash, err := HashToken(token)
	if err != nil {
		t.Fatal(err)
	}
	o := opts(t.TempDir(), "127.0.0.1:-1", discardLog())
	o.SatellitePermissions = Permissions{Publish: []string{testUtterance.With("terminal").Wire()}, Subscribe: []string{AllEvents}}
	o.Satellites = append([]Satellite{{
		Name: "voice-room", TokenHash: hash,
		Permissions: Permissions{Publish: []string{testUtterance.With("voice:room").Wire()}, Subscribe: []string{AllEvents}},
	}}, extra...)
	s := mustStart(t, o)
	t.Cleanup(s.Shutdown)
	return s, token
}

func dialAs(t *testing.T, s *Server, user, token string) (*Conn, error) {
	t.Helper()
	c, err := Dial(s.ClientURL(), DialOptions{Name: "sat", User: user, Token: token, Log: discardLog()})
	if err == nil {
		t.Cleanup(func() { c.Close() })
	}
	return c, err
}

func publishes(t *testing.T, c *Conn, s Subject[ping]) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := Publish(ctx, c, s, ping{N: 1})
	return err == nil
}

func TestNamedSatelliteSpeaksOnlyForItsChannel(t *testing.T) {
	s, token := startNamed(t)
	voice, err := dialAs(t, s, "voice-room", token)
	if err != nil {
		t.Fatalf("named satellite refused: %v", err)
	}
	if !publishes(t, voice, testUtterance.With("voice:room")) {
		t.Error("named satellite can't publish on its channel")
	}
	for _, other := range []string{"terminal", "whatsapp", "voice:kitchen"} {
		if publishes(t, voice, testUtterance.With(other)) {
			t.Errorf("named satellite published on %s", other)
		}
	}
	// The shared token keeps its own channel only.
	shared, err := dialAs(t, s, "", "test-token")
	if err != nil {
		t.Fatal(err)
	}
	if !publishes(t, shared, testUtterance.With("terminal")) || publishes(t, shared, testUtterance.With("voice:room")) {
		t.Error("shared token permissions wrong")
	}
	// Watching events still works.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := Subscribe(ctx, voice, testUtterance.With("terminal")); err != nil {
		t.Errorf("named satellite can't watch events: %v", err)
	}
}

func TestNamedSatelliteCredentialsDontMix(t *testing.T) {
	s, token := startNamed(t)
	for name, c := range map[string][2]string{
		"wrong token":             {"voice-room", "nope"},
		"named token as shared":   {"", token},
		"shared token as named":   {"voice-room", "test-token"},
		"unknown satellite":       {"voice-kitchen", token},
		"core user":               {"core", token},
		"empty token":             {"voice-room", ""},
		"token with extra spaces": {"voice-room", token + " "},
	} {
		if _, err := dialAs(t, s, c[0], c[1]); err == nil {
			t.Errorf("%s: connected", name)
		}
	}
}

func TestInvalidSatellitesStopTheBus(t *testing.T) {
	hash, _ := HashToken("x")
	for name, sat := range map[string]Satellite{
		"empty name":     {Name: "", TokenHash: hash},
		"core":           {Name: "core", TokenHash: hash},
		"shared user":    {Name: "satellite", TokenHash: hash},
		"dotted":         {Name: "voice.room", TokenHash: hash},
		"wildcard":       {Name: "voice*", TokenHash: hash},
		"spaces":         {Name: "voice room", TokenHash: hash},
		"long":           {Name: strings.Repeat("v", 65), TokenHash: hash},
		"plain token":    {Name: "voice-x", TokenHash: "not-a-hash"},
		"no token":       {Name: "voice-x"},
		"duplicate name": {Name: "voice-room", TokenHash: hash},
	} {
		o := opts(t.TempDir(), "127.0.0.1:-1", discardLog())
		o.Satellites = []Satellite{{Name: "voice-room", TokenHash: hash}, sat}
		if s, err := Start(context.Background(), o); err == nil {
			s.Shutdown()
			t.Errorf("%s: bus started", name)
		}
	}
}

func TestSubjectWithAndDecodeUnder(t *testing.T) {
	s := NewSubject[ping]("a.b")
	if got := s.With("c").Type(); got != "a.b.c" {
		t.Errorf("With = %q", got)
	}
	if got := s.With("c", "d").Wire(); got != "doggie.a.b.c.d" {
		t.Errorf("Wire = %q", got)
	}
	for _, bad := range []string{"", "x y", "*", ">", "x.", ".x"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("With(%q) did not panic", bad)
				}
			}()
			s.With(bad)
		}()
	}
	for typ, want := range map[string]string{"a.b.c": "c", "a.b.c.d": "c.d"} {
		ev, rest, err := DecodeUnder(Envelope{Meta: Meta{Type: typ}, Data: []byte(`{"n":3}`)}, s)
		if err != nil || rest != want || ev.Data.N != 3 {
			t.Errorf("DecodeUnder(%s) = %+v %q %v", typ, ev.Data, rest, err)
		}
	}
	for _, typ := range []string{"a.b", "a.bc", "x.a.b.c", "a.b."} {
		if _, _, err := DecodeUnder(Envelope{Meta: Meta{Type: typ}, Data: []byte(`{}`)}, s); err == nil {
			t.Errorf("DecodeUnder(%s) accepted", typ)
		}
	}
	if _, _, err := DecodeUnder(Envelope{Meta: Meta{Type: "a.b.c"}, Data: []byte(`{`)}, s); err == nil {
		t.Error("bad JSON accepted")
	}
}
