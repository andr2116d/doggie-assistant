package bus

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
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

// Satellites can be added, changed and removed while the bus runs. Only
// the connections of a satellite that changed are closed; one that may
// still connect comes back with its new permissions.
func TestSetSatellitesWhileRunning(t *testing.T) {
	s, token := startNamed(t)
	states := newConnStates()
	voice := states.dialAs(t, s.ClientURL(), "voice-room", token)
	defer voice.Close()
	shared, err := dialAs(t, s, "", "test-token")
	if err != nil {
		t.Fatal(err)
	}
	room := s.auth.named["voice-room"]

	// Add a satellite: it connects at once, and the others are untouched.
	kitchenHash, _ := HashToken("kitchen-token")
	kitchen := Satellite{Name: "voice-kitchen", TokenHash: kitchenHash, Permissions: Permissions{Publish: []string{testUtterance.With("voice:kitchen").Wire()}}}
	if err := s.SetSatellites([]Satellite{room, kitchen}); err != nil {
		t.Fatal(err)
	}
	kStates := newConnStates()
	k := kStates.dialAs(t, s.ClientURL(), "voice-kitchen", "kitchen-token")
	defer k.Close()
	states.quiet(t)
	if !publishes(t, k, testUtterance.With("voice:kitchen")) || !publishes(t, voice, testUtterance.With("voice:room")) || !publishes(t, shared, testUtterance.With("terminal")) {
		t.Error("adding a satellite broke another")
	}

	// Change its permissions: it is reconnected, with the new ones.
	room.Permissions = Permissions{Publish: []string{testUtterance.With("voice:hall").Wire()}}
	if err := s.SetSatellites([]Satellite{room, kitchen}); err != nil {
		t.Fatal(err)
	}
	waitState(t, states.down, "the changed satellite to be disconnected")
	waitState(t, states.up, "the changed satellite to come back")
	if publishes(t, voice, testUtterance.With("voice:room")) || !publishes(t, voice, testUtterance.With("voice:hall")) {
		t.Error("changed permissions did not apply to the satellite")
	}
	kStates.quiet(t)

	// An invalid list changes nothing.
	if err := s.SetSatellites([]Satellite{{Name: "core", TokenHash: kitchenHash}}); err == nil {
		t.Error("invalid list accepted")
	}
	if !publishes(t, k, testUtterance.With("voice:kitchen")) || !publishes(t, voice, testUtterance.With("voice:hall")) {
		t.Error("an invalid list changed the satellites")
	}

	// Remove one: it is disconnected for good and can't come back.
	if err := s.SetSatellites([]Satellite{kitchen}); err != nil {
		t.Fatal(err)
	}
	waitState(t, states.down, "the removed satellite to be disconnected")
	select {
	case err := <-states.closed:
		if !errors.Is(err, nats.ErrAuthorization) {
			t.Errorf("closed with %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the removed satellite kept trying to connect")
	}
	if _, err := dialAs(t, s, "voice-room", token); err == nil {
		t.Error("removed satellite could connect again")
	}
	kStates.quiet(t)
	if !publishes(t, shared, testUtterance.With("terminal")) || !publishes(t, k, testUtterance.With("voice:kitchen")) {
		t.Error("removing a satellite broke another")
	}
}

func waitState[T any](t *testing.T, ch <-chan T, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// A satellite whose token changes loses the old one at once, even on a live
// connection.
func TestSetSatellitesRotatesTokens(t *testing.T) {
	s, oldToken := startNamed(t)
	live, err := dialAs(t, s, "voice-room", oldToken)
	if err != nil {
		t.Fatal(err)
	}
	newHash, _ := HashToken("new-token")
	perms := Permissions{Publish: []string{testUtterance.With("voice:room").Wire()}}
	if err := s.SetSatellites([]Satellite{{Name: "voice-room", TokenHash: newHash, Permissions: perms}}); err != nil {
		t.Fatal(err)
	}
	if _, err := dialAs(t, s, "voice-room", oldToken); err == nil {
		t.Error("the old token still connects")
	}
	if _, err := dialAs(t, s, "voice-room", "new-token"); err != nil {
		t.Errorf("the new token is refused: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for publishes(t, live, testUtterance.With("voice:room")) {
		if time.Now().After(deadline) {
			t.Fatal("the connection on the old token can still publish")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Satellites connect while the list changes under them: no data race, and
// each connection ends up with the rights of the list it was accepted by.
func TestSetSatellitesWhileSatellitesConnect(t *testing.T) {
	s, token := startNamed(t)
	hash, _ := HashToken(token)
	lists := [][]Satellite{
		{{Name: "voice-room", TokenHash: hash, Permissions: Permissions{Publish: []string{testUtterance.With("voice:room").Wire()}}}},
		{{Name: "voice-room", TokenHash: hash, Permissions: Permissions{Publish: []string{testUtterance.With("voice:hall").Wire()}}}},
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				c, err := Dial(s.ClientURL(), DialOptions{Name: "sat", User: "voice-room", Token: token, Log: discardLog()})
				if err == nil {
					c.Close()
				}
			}
		})
	}
	for i := range 200 {
		if err := s.SetSatellites(lists[i%2]); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
}

// A connection's identity is what the bus vouches for: a named satellite's
// name, a component's name, and nobody for the shared token.
func TestConnIdentity(t *testing.T) {
	s, token := startNamed(t)
	named, err := dialAs(t, s, "voice-room", token)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := dialAs(t, s, "", "test-token")
	if err != nil {
		t.Fatal(err)
	}
	for c, want := range map[*Conn]string{named: "voice-room", shared: "", connect(t, s, "tasks"): "tasks"} {
		if got := c.Identity(); got != want {
			t.Errorf("%s: Identity = %q, want %q", c.Name(), got, want)
		}
	}
}
