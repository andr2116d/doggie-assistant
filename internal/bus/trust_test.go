package bus

import (
	"context"
	"crypto/tls"
	"errors"
	"github.com/nats-io/nats-server/v2/server"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

var (
	testManagerEvent = NewSubject[ping]("trusttest.manager")
	testWorkerEvent  = NewSubject[ping]("trusttest.worker")
	testManagerCmd   = NewCommand[echoReq, echoResp]("trusttest.manage")
	testWorkerCmd    = NewCommand[echoReq, echoResp]("trusttest.work")
	testBeat         = NewSignal[ping]("trusttest.beat")
)

// restricted lets satellites publish worker events and beats, request
// testManagerCmd and serve testWorkerCmd, and nothing else.
var restricted = Permissions{
	Publish:   []string{testWorkerEvent.Wire(), testManagerCmd.Wire(), testBeat.Wire()},
	Subscribe: []string{AllEvents, testWorkerCmd.Wire()},
}

func startRestricted(t *testing.T) (*Server, *Conn) {
	t.Helper()
	o := opts(t.TempDir(), "127.0.0.1:-1", discardLog())
	o.SatellitePermissions = restricted
	s := mustStart(t, o)
	t.Cleanup(s.Shutdown)
	sat, err := Dial(s.ClientURL(), DialOptions{Name: "satellite", Token: "test-token", Log: discardLog()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sat.Close() })
	return s, sat
}

func TestSatelliteCanDoWhatItIsAllowedTo(t *testing.T) {
	s, sat := startRestricted(t)
	core := connect(t, s, "core")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Publish an allowed event and watch it.
	events, err := Subscribe(ctx, sat, testWorkerEvent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Publish(ctx, sat, testWorkerEvent, ping{N: 1}); err != nil {
		t.Fatalf("publish allowed event: %v", err)
	}
	receive(t, events)

	// Request a command served by the core.
	serveFunc(t, ctx, core, testManagerCmd, func(echoReq) (echoResp, error) { return echoResp{By: "core"}, nil })
	if resp, err := Request(ctx, sat, testManagerCmd, echoReq{}); err != nil || resp.By != "core" {
		t.Fatalf("request allowed command: %+v, %v", resp, err)
	}

	// Serve a command the core requests, and reply to it.
	serveFunc(t, ctx, sat, testWorkerCmd, func(echoReq) (echoResp, error) { return echoResp{By: "satellite"}, nil })
	if resp, err := Request(ctx, core, testWorkerCmd, echoReq{}); err != nil || resp.By != "satellite" {
		t.Fatalf("serve allowed command: %+v, %v", resp, err)
	}

	// Read the event log.
	replay, err := Replay(ctx, sat, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if env := receive(t, replay); env.Type != testWorkerEvent.Type() {
		t.Errorf("replayed %s", env.Type)
	}

	// Emit a signal.
	beats, err := SubscribeSignal(ctx, core, testBeat)
	if err != nil {
		t.Fatal(err)
	}
	if err := Emit(sat, testBeat, ping{N: 7}); err != nil {
		t.Fatal(err)
	}
	if b := receive(t, beats); b.N != 7 {
		t.Errorf("beat = %+v", b)
	}
}

func TestSatelliteCannotForgeOrImpersonate(t *testing.T) {
	s, sat := startRestricted(t)
	core := connect(t, s, "core")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Forging an event it may not publish: never reaches the log.
	short, cancelShort := context.WithTimeout(ctx, 300*time.Millisecond)
	_, err := Publish(short, sat, testManagerEvent, ping{N: 1})
	cancelShort()
	if err == nil {
		t.Error("satellite published a forbidden event")
	}
	if n := eventLogInfo(t, core).State.Msgs; n != 0 {
		t.Errorf("event log has %d messages after a forbidden publish", n)
	}

	// Impersonating the core by serving its command: refused.
	if _, err := Serve(ctx, sat, testManagerCmd); err == nil {
		t.Error("satellite could serve a command reserved to the core")
	}

	// Sending a command it may not request: never delivered.
	var got int
	serveFunc(t, ctx, core, testWorkerCmd, func(echoReq) (echoResp, error) { got++; return echoResp{}, nil })
	short, cancelShort = context.WithTimeout(ctx, 300*time.Millisecond)
	_, err = Request(short, sat, NewCommand[echoReq, echoResp]("trusttest.other"), echoReq{})
	cancelShort()
	if err == nil {
		t.Error("satellite sent a forbidden command")
	}

	// Deleting the event log: refused, the stream survives.
	if _, err := sat.nc.Request("$JS.API.STREAM.DELETE."+EventLogStream, nil, 300*time.Millisecond); err == nil {
		t.Error("satellite request to delete the event log was answered")
	}
	if _, err := core.js.Stream(ctx, EventLogStream); err != nil {
		t.Errorf("event log gone after a satellite tried to delete it: %v", err)
	}
}

func TestCoreCredentialsOnlyWorkInProcess(t *testing.T) {
	s, _ := startRestricted(t)
	// Even knowing the core secret, a TCP client is refused.
	_, err := nats.Connect(s.ClientURL(), nats.UserInfo(coreUser, s.coreSecret))
	if err == nil {
		t.Fatal("core credentials accepted over TCP")
	}
	if !errors.Is(err, nats.ErrAuthorization) && !strings.Contains(strings.ToLower(err.Error()), "authorization") {
		t.Logf("refused with: %v", err)
	}
}

// A refused publish leaves its error behind; a later, allowed subscription to
// the same subject must not be mistaken for a refused one.
func TestRefusedPublishDoesNotBreakAllowedSubscribe(t *testing.T) {
	_, sat := startRestricted(t)
	sat.nc.Publish(testManagerEvent.Wire(), []byte("{}")) // forbidden publish
	sat.nc.Flush()
	// The server refuses it asynchronously: wait until the refusal is in.
	deadline := time.Now().Add(5 * time.Second)
	for !errors.Is(sat.nc.LastError(), nats.ErrPermissionViolation) {
		if time.Now().After(deadline) {
			t.Fatalf("the forbidden publish was not refused: last error %v", sat.nc.LastError())
		}
		time.Sleep(5 * time.Millisecond)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := Subscribe(ctx, sat, testManagerEvent); err != nil { // allowed by AllEvents
		t.Fatalf("allowed subscription refused: %v", err)
	}
}

// fakeClient is a connecting client, as the server shows it to Check.
type fakeClient struct {
	user, pass string
	addr       net.Addr
	kind       int
	registered *server.User
}

func (f *fakeClient) GetOpts() *server.ClientOpts {
	return &server.ClientOpts{Username: f.user, Password: f.pass}
}
func (f *fakeClient) GetTLSConnectionState() *tls.ConnectionState { return nil }
func (f *fakeClient) RegisterUser(u *server.User)                 { f.registered = u }
func (f *fakeClient) RemoteAddress() net.Addr                     { return f.addr }
func (f *fakeClient) GetNonce() []byte                            { return nil }
func (f *fakeClient) Kind() int                                   { return f.kind }
func (f *fakeClient) GetID() uint64                               { return 1 }

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }

func TestAuthenticatorCheck(t *testing.T) {
	roomHash, _ := HashToken("room-token")
	a, err := newAuthenticator("core-secret", "shared-token", Permissions{Publish: []string{"doggie.shared"}},
		[]Satellite{{Name: "room", TokenHash: roomHash, Permissions: Permissions{Publish: []string{"doggie.room"}}}})
	if err != nil {
		t.Fatal(err)
	}
	tcp := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}
	for _, tc := range []struct {
		name       string
		user, pass string
		addr       net.Addr
		kind       int
		ok         bool
		publish    string // a subject the user may publish, if accepted
	}{
		{"core in process", coreUser, "core-secret", pipeAddr{}, server.CLIENT, true, ""},
		{"core over the network", coreUser, "core-secret", tcp, server.CLIENT, false, ""},
		{"core, wrong secret", coreUser, "nope", pipeAddr{}, server.CLIENT, false, ""},
		{"core, no address", coreUser, "core-secret", nil, server.CLIENT, false, ""},
		{"shared token", satelliteUser, "shared-token", tcp, server.CLIENT, true, "doggie.shared"},
		{"shared token in process", satelliteUser, "shared-token", pipeAddr{}, server.CLIENT, false, ""},
		{"shared, wrong token", satelliteUser, "room-token", tcp, server.CLIENT, false, ""},
		{"named satellite", "room", "room-token", tcp, server.CLIENT, true, "doggie.room"},
		{"named, wrong token", "room", "shared-token", tcp, server.CLIENT, false, ""},
		{"named, empty token", "room", "", tcp, server.CLIENT, false, ""},
		{"named in process", "room", "room-token", pipeAddr{}, server.CLIENT, false, ""},
		{"unknown satellite", "kitchen", "room-token", tcp, server.CLIENT, false, ""},
		{"no user", "", "shared-token", tcp, server.CLIENT, false, ""},
		{"not a client", satelliteUser, "shared-token", tcp, server.LEAF, false, ""},
	} {
		c := &fakeClient{user: tc.user, pass: tc.pass, addr: tc.addr, kind: tc.kind}
		if got := a.Check(c); got != tc.ok {
			t.Errorf("%s: Check = %v, want %v", tc.name, got, tc.ok)
			continue
		}
		if !tc.ok {
			if c.registered != nil {
				t.Errorf("%s: refused but registered", tc.name)
			}
			continue
		}
		if c.registered == nil || c.registered.Username != tc.user {
			t.Errorf("%s: registered %+v", tc.name, c.registered)
			continue
		}
		perms := c.registered.Permissions
		if tc.publish == "" {
			if perms != nil {
				t.Errorf("%s: the core got restricted permissions", tc.name)
			}
		} else if perms == nil || !slices.Contains(perms.Publish.Allow, tc.publish) {
			t.Errorf("%s: permissions %+v", tc.name, perms)
		}
	}
}

// set reports exactly the satellites whose connections must go.
func TestAuthenticatorSetReportsChanges(t *testing.T) {
	h1, _ := HashToken("one")
	h2, _ := HashToken("two")
	sat := func(name, hash, pub string) Satellite {
		return Satellite{Name: name, TokenHash: hash, Permissions: Permissions{Publish: []string{pub}}}
	}
	a, err := newAuthenticator("c", "s", Permissions{}, []Satellite{sat("same", h1, "a"), sat("token", h1, "a"), sat("perms", h1, "a"), sat("gone", h1, "a")})
	if err != nil {
		t.Fatal(err)
	}
	changed, err := a.set([]Satellite{sat("same", h1, "a"), sat("token", h2, "a"), sat("perms", h1, "b"), sat("new", h1, "a")})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(changed)
	if !slices.Equal(changed, []string{"gone", "perms", "token"}) {
		t.Errorf("changed %q", changed)
	}
	if _, err := a.set([]Satellite{sat("x", h1, "a"), sat("x", h1, "a")}); err == nil {
		t.Error("repeated name accepted")
	}
	if _, err := a.set([]Satellite{sat(coreUser, h1, "a")}); err == nil {
		t.Error("the core's name accepted")
	}
	if _, err := a.set([]Satellite{sat("plain", "not-a-hash", "a")}); err == nil {
		t.Error("a plain token accepted")
	}
	if !a.Check(&fakeClient{user: "new", pass: "one", addr: &net.TCPAddr{}, kind: server.CLIENT}) {
		t.Error("an invalid list changed the satellites")
	}
}
