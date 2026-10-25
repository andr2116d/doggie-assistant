package bus

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
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

// users returns the NATS users for the core secret and the satellite token.
// The server only keeps bcrypt hashes of them.
func users(coreSecret, satelliteToken string, p Permissions) ([]*server.User, error) {
	coreHash, err := hashSecret(coreSecret)
	if err != nil {
		return nil, err
	}
	satelliteHash, err := hashSecret(satelliteToken)
	if err != nil {
		return nil, err
	}
	return []*server.User{
		{
			Username:               coreUser,
			Password:               coreHash,
			AllowedConnectionTypes: map[string]struct{}{jwt.ConnectionTypeInProcess: {}},
		},
		{
			Username:               satelliteUser,
			Password:               satelliteHash,
			AllowedConnectionTypes: map[string]struct{}{jwt.ConnectionTypeStandard: {}},
			Permissions: &server.Permissions{
				Publish:   &server.SubjectPermission{Allow: append(append([]string{}, satelliteBase.Publish...), p.Publish...)},
				Subscribe: &server.SubjectPermission{Allow: append(append([]string{}, satelliteBase.Subscribe...), p.Subscribe...)},
				// Lets a satellite answer the requests it serves (task runs
				// and controls) without publish rights on reply subjects.
				Response: &server.ResponsePermission{MaxMsgs: 1, Expires: time.Minute},
			},
		},
	}, nil
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
