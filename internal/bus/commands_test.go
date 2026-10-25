package bus

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type echoReq struct {
	Text string `json:"text"`
}

type echoResp struct {
	Text string `json:"text"`
	By   string `json:"by"`
}

var testEcho = NewCommand[echoReq, echoResp]("test.echo")

// serveFunc serves cmd in the background, answering with fn.
func serveFunc[Req, Resp any](t *testing.T, ctx context.Context, c *Conn, cmd Command[Req, Resp], fn func(Req) (Resp, error), opts ...ServeOption) {
	t.Helper()
	reqs, err := Serve(ctx, c, cmd, opts...)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for in := range reqs {
			resp, err := fn(in.Req)
			if err != nil {
				in.Fail(err)
				continue
			}
			in.Reply(resp)
		}
	}()
}

func TestRequestReply(t *testing.T) {
	s := startTestServer(t, "")
	server, client := connect(t, s, "server"), connect(t, s, "client")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveFunc(t, ctx, server, testEcho, func(r echoReq) (echoResp, error) {
		return echoResp{Text: strings.ToUpper(r.Text), By: "server"}, nil
	})

	resp, err := Request(ctx, client, testEcho, echoReq{Text: "hola"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "HOLA" || resp.By != "server" {
		t.Errorf("resp = %+v", resp)
	}
}

func TestRequestHandlerError(t *testing.T) {
	s := startTestServer(t, "")
	server, client := connect(t, s, "server"), connect(t, s, "client")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveFunc(t, ctx, server, testEcho, func(echoReq) (echoResp, error) {
		return echoResp{}, errors.New("unknown kind")
	})

	_, err := Request(ctx, client, testEcho, echoReq{})
	var remote *RemoteError
	if !errors.As(err, &remote) || remote.Message != "unknown kind" || remote.Command != "test.echo" {
		t.Fatalf("err = %v, want a RemoteError with the handler's message", err)
	}
}

func TestRequestWithoutHandlerFailsFast(t *testing.T) {
	s := startTestServer(t, "")
	client := connect(t, s, "client")
	start := time.Now()
	_, err := Request(context.Background(), client, testEcho, echoReq{})
	if !errors.Is(err, ErrNoHandler) {
		t.Fatalf("err = %v, want ErrNoHandler", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %s, want an immediate failure", elapsed)
	}
}

func TestRequestTimesOutWhenNobodyReplies(t *testing.T) {
	s := startTestServer(t, "")
	server, client := connect(t, s, "server"), connect(t, s, "client")
	serveCtx, cancelServe := context.WithCancel(context.Background())
	defer cancelServe()
	reqs, err := Serve(serveCtx, server, testEcho)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for range reqs { // receive but never reply
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := Request(ctx, client, testEcho, echoReq{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestServeRejectsUndecodableRequests(t *testing.T) {
	s := startTestServer(t, "")
	server, client := connect(t, s, "server"), connect(t, s, "client")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveFunc(t, ctx, server, testEcho, func(r echoReq) (echoResp, error) { return echoResp{}, nil })

	// Same subject, wrong request type.
	wrong := NewCommand[int, echoResp]("test.echo")
	_, err := Request(ctx, client, wrong, 42)
	var remote *RemoteError
	if !errors.As(err, &remote) || !strings.Contains(remote.Message, "bad request") {
		t.Fatalf("err = %v, want a bad request RemoteError", err)
	}
}

func TestQueueDeliversEachRequestOnce(t *testing.T) {
	s := startTestServer(t, "")
	client := connect(t, s, "client")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var a, b atomic.Int32
	for _, w := range []struct {
		name  string
		count *atomic.Int32
	}{{"worker-a", &a}, {"worker-b", &b}} {
		serveFunc(t, ctx, connect(t, s, w.name), testEcho, func(r echoReq) (echoResp, error) {
			w.count.Add(1)
			return echoResp{By: w.name}, nil
		}, Queue("workers"))
	}

	const n = 40
	for range n {
		if _, err := Request(ctx, client, testEcho, echoReq{}); err != nil {
			t.Fatal(err)
		}
	}
	if total := a.Load() + b.Load(); total != n {
		t.Errorf("handled %d requests, want exactly %d", total, n)
	}
	if a.Load() == 0 || b.Load() == 0 {
		t.Errorf("load not balanced: a=%d b=%d", a.Load(), b.Load())
	}
}

func TestCommandWithRoutesBySuffix(t *testing.T) {
	s := startTestServer(t, "")
	server, client := connect(t, s, "server"), connect(t, s, "client")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := NewCommand[echoReq, echoResp]("test.run")
	serveFunc(t, ctx, server, run.With("demo.sleep"), func(echoReq) (echoResp, error) {
		return echoResp{By: "sleeper"}, nil
	})

	if got := run.With("demo.sleep").Name(); got != "test.run.demo.sleep" {
		t.Errorf("Name = %q", got)
	}
	if resp, err := Request(ctx, client, run.With("demo.sleep"), echoReq{}); err != nil || resp.By != "sleeper" {
		t.Errorf("demo.sleep: %+v, %v", resp, err)
	}
	if _, err := Request(ctx, client, run.With("demo.other"), echoReq{}); !errors.Is(err, ErrNoHandler) {
		t.Errorf("demo.other: err = %v, want ErrNoHandler", err)
	}
}

func TestCommandsAreNotLogged(t *testing.T) {
	s := startTestServer(t, "")
	server, client := connect(t, s, "server"), connect(t, s, "client")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveFunc(t, ctx, server, testEcho, func(echoReq) (echoResp, error) { return echoResp{}, nil })

	for range 5 {
		if _, err := Request(ctx, client, testEcho, echoReq{}); err != nil {
			t.Fatal(err)
		}
	}
	if n := eventLogInfo(t, client).State.Msgs; n != 0 {
		t.Errorf("event log has %d messages, want 0: commands must not be logged", n)
	}
}

func TestServeClosesOnCancel(t *testing.T) {
	s := startTestServer(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	reqs, err := Serve(ctx, connect(t, s, "server"), testEcho)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case _, ok := <-reqs:
		if ok {
			t.Fatal("received a request after cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel not closed after cancel")
	}
}

func TestNewCommandValidation(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("NewCommand with a wildcard did not panic")
		}
	}()
	NewCommand[echoReq, echoResp]("task.*")
}

// A request that reaches a subscription in the instant between its ctx being
// cancelled and the unsubscribe must fail at once, not leave the requester
// waiting for its whole timeout.
func TestRequestToAStoppingHandlerFailsFast(t *testing.T) {
	s := startTestServer(t, "")
	server, client := connect(t, s, "server"), connect(t, s, "client")
	for i := range 200 {
		ctx, cancel := context.WithCancel(context.Background())
		reqs, err := Serve(ctx, server, testEcho)
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			for in := range reqs { // a real actor answers whatever it reads
				in.Reply(echoResp{})
			}
		}()
		cancel()

		rctx, rcancel := context.WithTimeout(context.Background(), time.Second)
		_, err = Request(rctx, client, testEcho, echoReq{})
		rcancel()
		var remote *RemoteError
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			t.Fatalf("iteration %d: request timed out instead of failing fast", i)
		case err == nil, errors.Is(err, ErrNoHandler):
		case errors.As(err, &remote) && remote.Message == ErrHandlerStopped.Error():
		default:
			t.Fatalf("iteration %d: unexpected error %v", i, err)
		}
	}
}
