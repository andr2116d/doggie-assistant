// Package mcp is DOGGIE's MCP server: the only tools the LLM brain can use.
// The brain runs it in-process, over HTTP on the loopback
// interface, and the tools reach the rest of DOGGIE through the bus.
//
// Every LLM process is a Client with its own bearer token. A token is useless
// on its own: tools only run while its client is bound to a conversation
// turn, which tells them which thread the actions belong to. The LLM never
// chooses the thread.
//
// Every tool declares a risk level. Sensitive actions need a person's
// confirmation on a trusted channel, which this server enforces, not the
// prompt.
package mcp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
)

const (
	// maxRequestBytes bounds an HTTP request body.
	maxRequestBytes = 1 << 20
	// shutdownTimeout bounds how long Serve waits for requests in flight.
	shutdownTimeout = 2 * time.Second
)

// ErrNoTurn is returned to tools called outside a conversation turn.
var ErrNoTurn = errors.New("no conversation turn is active for this client")

// Turn is the conversation turn a client serves.
type Turn struct {
	ThreadID string
	Channel  string
	// Said are the event IDs of the sentences the turn answers: the tasks
	// it starts were caused by them.
	Said []string
	// Resumed are those of them a restart left unanswered: the turn that
	// died may have acted on them already.
	Resumed []string
}

// Server is DOGGIE's MCP server.
type Server struct {
	conn    *bus.Conn
	log     *slog.Logger
	mcp     *sdk.Server
	kinds   map[string]kind
	ordered []events.TaskKind // kinds in catalog order, for the tool description
	tools   map[string]events.Risk

	ln  net.Listener
	url string

	mu      sync.Mutex
	clients map[string]*Client // by token hash
}

// Client is one LLM process's access to the tools.
type Client struct {
	s     *Server
	token string
	hash  string // hashToken(token)
	turn  *Turn  // guarded by s.mu; nil when no turn is active
}

// Listen returns a server listening on a random loopback port. Serve must be
// called to handle requests.
func Listen(conn *bus.Conn, log *slog.Logger) (*Server, error) {
	return listen(conn, log, events.TaskKinds())
}

func listen(conn *bus.Conn, log *slog.Logger, kindList []events.TaskKind) (*Server, error) {
	kinds, err := compileKinds(kindList)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("mcp: listen: %w", err)
	}
	s := &Server{
		conn:    conn,
		log:     log,
		kinds:   kinds,
		ordered: kindList,
		tools:   make(map[string]events.Risk),
		ln:      ln,
		url:     "http://" + ln.Addr().String() + "/mcp",
		clients: make(map[string]*Client),
	}
	s.mcp = sdk.NewServer(&sdk.Implementation{Name: "doggie", Version: "0.1.0"}, nil)
	s.addTaskTools()
	return s, nil
}

// URL is the server's MCP endpoint.
func (s *Server) URL() string { return s.url }

// Tools returns the risk level of every tool, by name.
func (s *Server) Tools() map[string]events.Risk {
	return maps.Clone(s.tools)
}

// Serve handles requests until ctx is cancelled, then stops the server and
// revokes every client. Call it once.
func (s *Server) Serve(ctx context.Context) error {
	// Stateless: an LLM process is disposable, so the server keeps no MCP
	// session for it that could outlive it.
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return s.mcp }, &sdk.StreamableHTTPOptions{
		Stateless: true,
		Logger:    s.log,
	})
	authed := auth.RequireBearerToken(s.verify, &auth.RequireBearerTokenOptions{AllowMissingExpiration: true})(handler)
	mux := http.NewServeMux()
	mux.Handle("/mcp", http.MaxBytesHandler(authed, maxRequestBytes))
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
		case <-done:
			return
		}
		sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil {
			srv.Close()
		}
	}()

	err := srv.Serve(s.ln)
	s.mu.Lock()
	clear(s.clients)
	s.mu.Unlock()
	if errors.Is(err, http.ErrServerClosed) {
		return ctx.Err()
	}
	return fmt.Errorf("mcp: serve: %w", err)
}

// NewClient registers a client with a fresh token. Close it when its LLM
// process exits.
func (s *Server) NewClient() (*Client, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("mcp: token: %w", err)
	}
	token := hex.EncodeToString(b)
	c := &Client{s: s, token: token, hash: hashToken(token)}
	s.mu.Lock()
	s.clients[c.hash] = c
	s.mu.Unlock()
	return c, nil
}

// hashToken keys clients by a hash of their token, so a lookup's timing
// reveals nothing about valid tokens, and the key can identify the client
// without revealing its token.
func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func (s *Server) verify(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	s.mu.Lock()
	c, ok := s.clients[hashToken(token)]
	s.mu.Unlock()
	if !ok {
		return nil, auth.ErrInvalidToken
	}
	return &auth.TokenInfo{UserID: c.hash}, nil
}

// turnOf returns the turn of the client that sent req.
func (s *Server) turnOf(req *sdk.CallToolRequest) (Turn, error) {
	if req.Extra == nil || req.Extra.TokenInfo == nil {
		return Turn{}, ErrNoTurn
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.clients[req.Extra.TokenInfo.UserID]
	if !ok || c.turn == nil {
		return Turn{}, ErrNoTurn
	}
	return *c.turn, nil
}

// Token is the client's bearer token.
func (c *Client) Token() string { return c.token }

// Bind lets the client's tools act for turn, until Unbind.
func (c *Client) Bind(turn Turn) error {
	if turn.ThreadID == "" {
		return errors.New("mcp: a turn needs a thread")
	}
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	if _, ok := c.s.clients[c.hash]; !ok {
		return errors.New("mcp: client is closed")
	}
	c.turn = &turn
	return nil
}

// Unbind ends the client's turn: its tools refuse to act until the next Bind.
func (c *Client) Unbind() {
	c.s.mu.Lock()
	c.turn = nil
	c.s.mu.Unlock()
}

// Close revokes the client's token.
func (c *Client) Close() {
	c.s.mu.Lock()
	c.turn = nil
	delete(c.s.clients, c.hash)
	c.s.mu.Unlock()
}
