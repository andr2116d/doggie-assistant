package bus

import (
	"context"
	"errors"
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
	time.Sleep(50 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := Subscribe(ctx, sat, testManagerEvent); err != nil { // allowed by AllEvents
		t.Fatalf("allowed subscription refused: %v", err)
	}
}
