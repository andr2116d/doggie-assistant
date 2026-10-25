// Package bus is DOGGIE's event bus: an embedded NATS server whose events are
// persisted in a JetStream log, plus typed helpers to publish and subscribe.
//
// Components inside the process connect without networking, with full
// access. Satellites in other processes or machines connect over TCP with the
// bus token and restricted permissions (see trust.go).
package bus

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	// subjectPrefix namespaces every DOGGIE event subject.
	subjectPrefix = "doggie."
	// EventLogStream is the JetStream stream that stores every event.
	EventLogStream = "EVENTS"
)

// Options configures the embedded server.
type Options struct {
	// Listen is host:port for TCP clients. Empty means in-process only.
	Listen string
	// Token authenticates satellites (TCP clients). Required. In-process
	// components use a separate secret that never leaves memory.
	Token string
	// SatellitePermissions is what satellites may do beyond receiving replies
	// and reading the event log.
	SatellitePermissions Permissions
	// Satellites are named satellites, each with its own token and
	// permissions.
	Satellites []Satellite
	// StoreDir is where JetStream persists the event log.
	StoreDir string
	// MaxAge and MaxBytes bound the event log.
	MaxAge   time.Duration
	MaxBytes int64
	// TLS, when set, is required for every TCP client. In-process
	// connections never touch the network and are not affected.
	TLS *tls.Config
	Log *slog.Logger
}

// Server is the embedded NATS server.
type Server struct {
	ns         *server.Server
	coreSecret string
	log        *slog.Logger

	// What SetSatellites needs to rebuild the users.
	reloadMu    sync.Mutex
	sopts       server.Options
	token       string
	permissions Permissions
}

// Start runs the embedded server and makes sure the event log stream exists.
func Start(ctx context.Context, opts Options) (*Server, error) {
	if opts.Token == "" {
		return nil, errors.New("bus: token is required")
	}
	coreSecret, err := randomSecret()
	if err != nil {
		return nil, fmt.Errorf("bus: generate core secret: %w", err)
	}
	natsUsers, err := users(coreSecret, opts.Token, opts.SatellitePermissions, opts.Satellites)
	if err != nil {
		return nil, err
	}
	sopts := &server.Options{
		ServerName:             "doggie",
		DontListen:             opts.Listen == "",
		Users:                  natsUsers,
		JetStream:              true,
		StoreDir:               opts.StoreDir,
		NoSigs:                 true,
		DisableJetStreamBanner: true,
	}
	if opts.TLS != nil {
		sopts.TLS = true
		sopts.TLSConfig = opts.TLS
		sopts.TLSTimeout = 2
	}
	if opts.Listen != "" {
		host, port, err := net.SplitHostPort(opts.Listen)
		if err != nil {
			return nil, fmt.Errorf("bus: listen %q: %w", opts.Listen, err)
		}
		sopts.Host = host
		if sopts.Port, err = strconv.Atoi(port); err != nil {
			return nil, fmt.Errorf("bus: listen port %q: %w", port, err)
		}
	}

	ns, err := server.NewServer(sopts)
	if err != nil {
		return nil, fmt.Errorf("bus: %w", err)
	}
	logger := natsLogger{log: opts.Log, fatal: make(chan string, 1)}
	ns.SetLoggerV2(logger, false, false, false)
	ns.Start()
	if err := waitReady(ns, logger.fatal, 10*time.Second); err != nil {
		ns.Shutdown()
		return nil, err
	}

	s := &Server{ns: ns, coreSecret: coreSecret, log: opts.Log, sopts: *sopts, token: opts.Token, permissions: opts.SatellitePermissions}
	if err := s.ensureEventLog(ctx, opts.MaxAge, opts.MaxBytes); err != nil {
		s.Shutdown()
		return nil, err
	}
	return s, nil
}

// waitReady waits until the server accepts connections. It fails fast when
// the server reports a fatal error, e.g. the listen address is in use.
func waitReady(ns *server.Server, fatal <-chan string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case msg := <-fatal:
			return fmt.Errorf("bus: %s", msg)
		default:
		}
		if ns.ReadyForConnections(50 * time.Millisecond) {
			return nil
		}
	}
	return fmt.Errorf("bus: server not ready after %s", timeout)
}

func (s *Server) ensureEventLog(ctx context.Context, maxAge time.Duration, maxBytes int64) error {
	nc, err := s.dial("bus-setup")
	if err != nil {
		return err
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		return fmt.Errorf("bus: jetstream: %w", err)
	}
	_, err = js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:     EventLogStream,
		Subjects: []string{subjectPrefix + ">"},
		Storage:  jetstream.FileStorage,
		Discard:  jetstream.DiscardOld,
		MaxAge:   maxAge,
		MaxBytes: maxBytes,
	})
	if err != nil {
		return fmt.Errorf("bus: create event log stream: %w", err)
	}
	return nil
}

// Connect opens an in-process connection for the named component. Events it
// publishes carry that name as their source.
func (s *Server) Connect(name string) (*Conn, error) {
	return newConn(name, s.log, connHooks{}, func(opts ...nats.Option) (*nats.Conn, error) {
		return s.dial(name, opts...)
	})
}

func (s *Server) dial(name string, extra ...nats.Option) (*nats.Conn, error) {
	nc, err := nats.Connect("", append([]nats.Option{
		nats.InProcessServer(s.ns),
		nats.UserInfo(coreUser, s.coreSecret),
		nats.Name(name),
		errorHandler(s.log, name),
	}, extra...)...)
	if err != nil {
		return nil, fmt.Errorf("bus: connect %s: %w", name, err)
	}
	return nc, nil
}

// ClientURL returns the TCP URL satellites use. It is empty when the server
// only accepts in-process connections.
func (s *Server) ClientURL() string {
	if s.ns.Addr() == nil {
		return ""
	}
	return s.ns.ClientURL()
}

// SetSatellites replaces the named satellites while the bus runs. Connected
// satellites are checked again: one that was removed, or whose token changed,
// is disconnected, and permissions apply at once. An invalid list changes
// nothing.
func (s *Server) SetSatellites(named []Satellite) error {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	users, err := users(s.coreSecret, s.token, s.permissions, named)
	if err != nil {
		return err
	}
	opts := s.sopts
	opts.Users = users
	if addr, ok := s.ns.Addr().(*net.TCPAddr); ok {
		opts.Port = addr.Port // a random port (-1) was resolved at start
	}
	if err := s.ns.ReloadOptions(&opts); err != nil {
		return fmt.Errorf("bus: reload satellites: %w", err)
	}
	s.sopts = opts
	return nil
}

// Shutdown stops the server and waits until the event log is flushed.
func (s *Server) Shutdown() {
	s.ns.Shutdown()
	s.ns.WaitForShutdown()
}

// ErrNoServer means nothing answered at the bus URL: DOGGIE isn't running
// there.
var ErrNoServer = nats.ErrNoServers

// DialOptions configures a TCP connection to the bus.
type DialOptions struct {
	// Name identifies the client. Its events carry it as their source.
	Name string
	// User is a named satellite (see Options.Satellites). Empty means the
	// shared satellite token.
	User  string
	Token string
	// CAFile is the bus CA certificate. Required when the bus uses TLS.
	CAFile string
	Log    *slog.Logger
	// OnDisconnect and OnReconnect are called when the connection to the
	// bus is lost and when it is back; subscriptions resume by themselves.
	// Events published meanwhile are missed (Replay can fetch them).
	// OnClosed is called if the connection ends for good, e.g. when the bus
	// refuses its credentials after its satellite was removed; err says
	// why, if known.
	//
	// Closing the connection with Close is neither a disconnect nor reported
	// to OnClosed. The callbacks run on the connection's callback goroutine,
	// so they must not block.
	OnDisconnect func(err error)
	OnReconnect  func()
	OnClosed     func(err error)
}

// Dial connects to a DOGGIE bus over TCP, for satellites and tools running in
// another process.
func Dial(url string, opts DialOptions) (*Conn, error) {
	user := opts.User
	if user == "" {
		user = satelliteUser
	}
	natsOpts := []nats.Option{
		nats.UserInfo(user, opts.Token),
		nats.Name(opts.Name),
		nats.MaxReconnects(-1),
		errorHandler(opts.Log, opts.Name),
	}
	if opts.CAFile != "" {
		natsOpts = append(natsOpts, nats.RootCAs(opts.CAFile))
	}
	hooks := connHooks{onDisconnect: opts.OnDisconnect, onReconnect: opts.OnReconnect, onClosed: opts.OnClosed}
	return newConn(opts.Name, opts.Log, hooks, func(extra ...nats.Option) (*nats.Conn, error) {
		nc, err := nats.Connect(url, append(natsOpts, extra...)...)
		if errors.Is(err, nats.ErrNoServers) {
			return nil, fmt.Errorf("bus: no server at %s (is doggie running?): %w", url, err)
		}
		if err != nil {
			return nil, fmt.Errorf("bus: dial %s: %w", url, err)
		}
		return nc, nil
	})
}

func errorHandler(log *slog.Logger, name string) nats.Option {
	return nats.ErrorHandler(func(_ *nats.Conn, sub *nats.Subscription, err error) {
		attrs := []any{"conn", name, "err", err}
		if sub != nil {
			attrs = append(attrs, "subject", sub.Subject)
		}
		log.Warn("bus error", attrs...)
	})
}

// natsLogger routes the NATS server's logs to slog. Notices are routine
// startup chatter, so they go to debug. The first fatal error is also sent on
// fatal, so Start can fail with it.
type natsLogger struct {
	log   *slog.Logger
	fatal chan string
}

func (l natsLogger) Noticef(format string, v ...any) {
	l.log.Debug(fmt.Sprintf(format, v...), "src", "nats")
}
func (l natsLogger) Warnf(format string, v ...any) {
	l.log.Warn(fmt.Sprintf(format, v...), "src", "nats")
}
func (l natsLogger) Fatalf(format string, v ...any) {
	msg := fmt.Sprintf(format, v...)
	l.log.Error(msg, "src", "nats")
	select {
	case l.fatal <- msg:
	default:
	}
}
func (l natsLogger) Errorf(format string, v ...any) {
	l.log.Error(fmt.Sprintf(format, v...), "src", "nats")
}
func (l natsLogger) Debugf(format string, v ...any) {
	l.log.Debug(fmt.Sprintf(format, v...), "src", "nats")
}
func (l natsLogger) Tracef(format string, v ...any) {
	l.log.Debug(fmt.Sprintf(format, v...), "src", "nats")
}
