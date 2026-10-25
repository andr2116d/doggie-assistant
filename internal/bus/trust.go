package bus

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/jwt/v2"
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

// users returns the NATS users for the core secret, the satellite token and
// the named satellites. The server only keeps bcrypt hashes of the secrets.
func users(coreSecret, satelliteToken string, p Permissions, named []Satellite) ([]*server.User, error) {
	coreHash, err := hashSecret(coreSecret)
	if err != nil {
		return nil, err
	}
	satelliteHash, err := hashSecret(satelliteToken)
	if err != nil {
		return nil, err
	}
	satellite := func(name, hash string, p Permissions) *server.User {
		return &server.User{
			Username:               name,
			Password:               hash,
			AllowedConnectionTypes: map[string]struct{}{jwt.ConnectionTypeStandard: {}},
			Permissions: &server.Permissions{
				Publish:   &server.SubjectPermission{Allow: append(append([]string{}, satelliteBase.Publish...), p.Publish...)},
				Subscribe: &server.SubjectPermission{Allow: append(append([]string{}, satelliteBase.Subscribe...), p.Subscribe...)},
				// Lets a satellite answer the requests it serves (task runs
				// and controls) without publish rights on reply subjects.
				Response: &server.ResponsePermission{MaxMsgs: 1, Expires: time.Minute},
			},
		}
	}
	out := []*server.User{
		{
			Username:               coreUser,
			Password:               coreHash,
			AllowedConnectionTypes: map[string]struct{}{jwt.ConnectionTypeInProcess: {}},
		},
		satellite(satelliteUser, satelliteHash, p),
	}
	seen := map[string]bool{}
	for _, s := range named {
		if !ValidSatelliteName(s.Name) || seen[s.Name] {
			return nil, fmt.Errorf("bus: invalid or repeated satellite name %q", s.Name)
		}
		if !strings.HasPrefix(s.TokenHash, "$2") {
			return nil, fmt.Errorf("bus: satellite %q has no bcrypt token hash", s.Name)
		}
		seen[s.Name] = true
		out = append(out, satellite(s.Name, s.TokenHash, s.Permissions))
	}
	return out, nil
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
