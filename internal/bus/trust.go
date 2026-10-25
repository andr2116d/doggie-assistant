package bus

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"golang.org/x/crypto/bcrypt"
)

// The bus has two kinds of clients, with separate credentials:
//
//   - core: the components inside the doggie process. They authenticate with
//     a random secret that only lives in memory, and may only connect
//     in-process. Full access.
//   - satellite: anything connecting over TCP (satellites, tools such as
//     doggie events or doggie task). They authenticate with the bus token
//     and are limited to the subjects in Options.SatellitePermissions.
const (
	coreUser      = "core"
	satelliteUser = "satellite"
)

// AllEvents matches every DOGGIE event subject, e.g. to let satellites watch
// them.
const AllEvents = subjectPrefix + ">"

// Permissions restrict what satellite connections may do, as NATS subjects
// or patterns (see the Wire methods of Subject, Command and Signal).
type Permissions struct {
	// Publish lists what satellites may publish events or signals on, or
	// send requests to.
	Publish []string
	// Subscribe lists what satellites may subscribe to or serve.
	Subscribe []string
}

// satelliteBase is what every satellite needs whatever its role: receiving
// replies to its requests, and reading the event log (Replay).
var satelliteBase = Permissions{
	Publish: []string{
		"$JS.API.CONSUMER.CREATE." + EventLogStream,
		"$JS.API.CONSUMER.CREATE." + EventLogStream + ".>",
		"$JS.API.CONSUMER.MSG.NEXT." + EventLogStream + ".>",
		"$JS.API.CONSUMER.INFO." + EventLogStream + ".>",
		"$JS.API.CONSUMER.DELETE." + EventLogStream + ".>",
		"$JS.API.STREAM.INFO." + EventLogStream,
	},
	Subscribe: []string{"_INBOX.>"},
}

// Satellite is a named satellite with its own token and permissions, e.g. a
// voice satellite that may only speak for its own channel. It connects with
// DialOptions.User set to its name.
type Satellite struct {
	Name string
	// TokenHash is the bcrypt hash of its token (see HashToken).
	TokenHash   string
	Permissions Permissions
}

// ValidSatelliteName reports whether name can name a satellite: a valid
// name token that isn't one of the bus's own users.
func ValidSatelliteName(name string) bool {
	return ValidName(name) && !strings.Contains(name, ".") && len(name) <= 64 && name != coreUser && name != satelliteUser
}

// HashToken returns the bcrypt hash a Satellite keeps of its token.
func HashToken(token string) (string, error) { return hashSecret(token) }

// authenticator checks every connection against the current credentials,
// so satellites can change without reloading the NATS server: a reload
// re-checks connected clients while new ones authenticate, and the server
// races with itself doing so.
type authenticator struct {
	coreHash, sharedHash string
	shared               *server.Permissions

	// mu is held while a connection is checked, so that once set returns
	// no connection can still be accepted by the old list.
	mu    sync.RWMutex
	named map[string]Satellite
}

func newAuthenticator(coreSecret, sharedToken string, shared Permissions, named []Satellite) (*authenticator, error) {
	coreHash, err := hashSecret(coreSecret)
	if err != nil {
		return nil, err
	}
	sharedHash, err := hashSecret(sharedToken)
	if err != nil {
		return nil, err
	}
	a := &authenticator{coreHash: coreHash, sharedHash: sharedHash, shared: natsPermissions(shared)}
	if _, err := a.set(named); err != nil {
		return nil, err
	}
	return a, nil
}

// natsPermissions turns a satellite's permissions into the server's.
func natsPermissions(p Permissions) *server.Permissions {
	return &server.Permissions{
		Publish:   &server.SubjectPermission{Allow: append(append([]string{}, satelliteBase.Publish...), p.Publish...)},
		Subscribe: &server.SubjectPermission{Allow: append(append([]string{}, satelliteBase.Subscribe...), p.Subscribe...)},
		// Lets a satellite answer the requests it serves (task runs and
		// controls) without publish rights on reply subjects.
		Response: &server.ResponsePermission{MaxMsgs: 1, Expires: time.Minute},
	}
}

// set replaces the named satellites. It returns the names whose token or
// permissions changed or that are gone: their connections must be closed.
// An invalid list changes nothing.
func (a *authenticator) set(named []Satellite) (changed []string, err error) {
	next := make(map[string]Satellite, len(named))
	for _, s := range named {
		if !ValidSatelliteName(s.Name) || next[s.Name].Name != "" {
			return nil, fmt.Errorf("bus: invalid or repeated satellite name %q", s.Name)
		}
		if !strings.HasPrefix(s.TokenHash, "$2") {
			return nil, fmt.Errorf("bus: satellite %q has no bcrypt token hash", s.Name)
		}
		next[s.Name] = s
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for name, old := range a.named {
		if s, ok := next[name]; !ok || s.TokenHash != old.TokenHash || !slices.Equal(s.Permissions.Publish, old.Permissions.Publish) || !slices.Equal(s.Permissions.Subscribe, old.Permissions.Subscribe) {
			changed = append(changed, name)
		}
	}
	a.named = next
	return changed, nil
}

// Check implements server.Authentication. The core may only connect in
// process, and satellites only over the network.
func (a *authenticator) Check(c server.ClientAuthentication) bool {
	if c.Kind() != server.CLIENT {
		return false
	}
	opts := c.GetOpts()
	addr := c.RemoteAddress()
	inProcess := addr != nil && addr.Network() == "pipe"
	switch user := opts.Username; {
	case user == coreUser:
		if !inProcess || !matches(a.coreHash, opts.Password) {
			return false
		}
		c.RegisterUser(&server.User{Username: coreUser})
		return true
	case inProcess:
		return false
	case user == satelliteUser:
		if !matches(a.sharedHash, opts.Password) {
			return false
		}
		c.RegisterUser(&server.User{Username: satelliteUser, Permissions: a.shared})
		return true
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	s, ok := a.named[opts.Username]
	if !ok || !matches(s.TokenHash, opts.Password) {
		return false
	}
	c.RegisterUser(&server.User{Username: s.Name, Permissions: natsPermissions(s.Permissions)})
	return true
}

func matches(hash, secret string) bool {
	return secret != "" && bcrypt.CompareHashAndPassword([]byte(hash), []byte(secret)) == nil
}

// hashSecret hashes a secret for the server's user list. The minimum cost is
// enough: the secrets are random 256-bit values, so their strength comes from
// their entropy, and every connection pays the cost.
func hashSecret(secret string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.MinCost)
	if err != nil {
		return "", fmt.Errorf("bus: hash secret: %w", err)
	}
	return string(h), nil
}

func randomSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
