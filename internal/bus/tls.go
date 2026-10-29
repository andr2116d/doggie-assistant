package bus

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// DOGGIE runs its own small certificate authority for the bus. A satellite
// only needs to trust one CA file. The server certificate can be reissued
// (new IP, expiry) without touching any satellite.

// CAEnv overrides the CA file a client trusts, e.g. for satellites on other
// machines.
const CAEnv = "DOGGIE_BUS_CA"

const (
	caValidity     = 10 * 365 * 24 * time.Hour
	serverValidity = 365 * 24 * time.Hour
	// renewBefore is how long before expiry the server certificate is reissued.
	renewBefore = 30 * 24 * time.Hour
)

// TLSFiles are the PEM files of the bus certificate authority and server.
type TLSFiles struct {
	CA, CAKey, Cert, Key string
}

// TLSFilesIn returns the file layout inside dir.
func TLSFilesIn(dir string) TLSFiles {
	return TLSFiles{
		CA:    filepath.Join(dir, "ca.pem"),
		CAKey: filepath.Join(dir, "ca-key.pem"),
		Cert:  filepath.Join(dir, "server.pem"),
		Key:   filepath.Join(dir, "server-key.pem"),
	}
}

// EnsureTLS makes sure dir holds a CA and a server certificate valid for every
// host. The CA is created once and kept. The server certificate is reissued
// when it expires within 30 days, misses a host or was not signed by the CA.
func EnsureTLS(dir string, hosts []string, now time.Time) (TLSFiles, error) {
	files := TLSFilesIn(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return files, fmt.Errorf("bus: create tls dir: %w", err)
	}
	ca, caKey, err := loadOrCreateCA(files, now)
	if err != nil {
		return files, err
	}
	if serverCertValid(files.Cert, ca, hosts, now) {
		return files, nil
	}
	return files, issueServerCert(files, ca, caKey, hosts, now)
}

// ServerTLS returns the NATS server's TLS config, serving the certificate in
// dir, made by EnsureTLS for hosts(). DOGGIE runs for months and its
// addresses change, so the certificate is reissued while it runs: when it
// nears expiry, or when a satellite reaches the server through a name or
// address it doesn't cover and hosts() now lists, e.g. a Tailscale address
// that came up after DOGGIE did. Names that hosts() doesn't list are never
// added, so a client can't have certificates made for whatever it asks.
func ServerTLS(dir string, hosts func() []string, log *slog.Logger) (*tls.Config, error) {
	s := &certSource{dir: dir, hosts: hosts, log: log, now: time.Now}
	if err := s.load(hosts()); err != nil {
		return nil, err
	}
	// No Certificates: with one, crypto/tls skips GetCertificate for
	// clients that dial an IP, which send no server name.
	return &tls.Config{GetCertificate: s.get, MinVersion: tls.VersionTLS13}, nil
}

// certSource serves the server certificate and reissues it when needed.
// Handshakes run concurrently: mu guards the fields below it.
type certSource struct {
	dir   string
	hosts func() []string
	log   *slog.Logger
	now   func() time.Time

	mu      sync.Mutex
	cert    *tls.Certificate
	covered []string // the hosts cert was made for
}

func (s *certSource) get(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	asked := askedHost(hello)
	expiring := s.now().Add(renewBefore).After(s.cert.Leaf.NotAfter)
	if !expiring && (asked == "" || s.cert.Leaf.VerifyHostname(asked) == nil) {
		return s.cert, nil
	}
	hosts := s.hosts()
	if !expiring && slices.Equal(hosts, s.covered) {
		return s.cert, nil // not one of DOGGIE's names: the client fails to verify it
	}
	if err := s.load(hosts); err != nil {
		s.log.Warn("bus: could not reissue the server certificate; serving the old one", "err", err)
		return s.cert, nil
	}
	s.log.Info("bus: server certificate reissued", "hosts", hosts, "for", asked)
	return s.cert, nil
}

// load makes sure dir holds a certificate for hosts and loads it. A key
// that doesn't match its certificate, e.g. after a crash between writing
// one and the other, is replaced with a new pair.
func (s *certSource) load(hosts []string) error {
	files, err := EnsureTLS(s.dir, hosts, s.now())
	if err != nil {
		return err
	}
	cert, err := tls.LoadX509KeyPair(files.Cert, files.Key)
	if err != nil {
		if rerr := os.Remove(files.Cert); rerr != nil {
			return fmt.Errorf("bus: load server certificate: %w", err)
		}
		if files, err = EnsureTLS(s.dir, hosts, s.now()); err != nil {
			return err
		}
		if cert, err = tls.LoadX509KeyPair(files.Cert, files.Key); err != nil {
			return fmt.Errorf("bus: load server certificate: %w", err)
		}
	}
	if cert.Leaf == nil { // only filled in by default
		if cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0]); err != nil {
			return fmt.Errorf("bus: parse server certificate: %w", err)
		}
	}
	s.cert, s.covered = &cert, hosts
	return nil
}

// askedHost is the name or address a client reached the server by: the
// name it asked for, or, as clients that dial an IP send none, the local
// address of the connection.
func askedHost(hello *tls.ClientHelloInfo) string {
	if hello.ServerName != "" {
		return hello.ServerName
	}
	if hello.Conn == nil {
		return ""
	}
	if addr, ok := hello.Conn.LocalAddr().(*net.TCPAddr); ok {
		return addr.IP.String()
	}
	return ""
}

// TLSHosts lists the names and addresses the server certificate must cover:
// localhost, this machine's hostname, its interface IPs (LAN and Tailscale
// included), a specific listen host, and any extra hosts from the config.
func TLSHosts(listen string, extra []string) []string {
	hosts := []string{"localhost", "127.0.0.1", "::1"}
	if name, err := os.Hostname(); err == nil && name != "" {
		hosts = append(hosts, name)
		if !strings.Contains(name, ".") {
			hosts = append(hosts, name+".local")
		}
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.IsGlobalUnicast() {
				hosts = append(hosts, ipnet.IP.String())
			}
		}
	}
	if host, _, err := net.SplitHostPort(listen); err == nil {
		if ip := net.ParseIP(host); host != "" && (ip == nil || !ip.IsUnspecified()) {
			hosts = append(hosts, host)
		}
	}
	hosts = append(hosts, extra...)
	slices.Sort(hosts)
	return slices.Compact(hosts)
}

func loadOrCreateCA(files TLSFiles, now time.Time) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	cert, certErr := readCert(files.CA)
	key, keyErr := readKey(files.CAKey)
	if certErr == nil && keyErr == nil {
		return cert, key, nil
	}
	if !errors.Is(certErr, fs.ErrNotExist) && certErr != nil {
		return nil, nil, certErr
	}
	if !errors.Is(keyErr, fs.ErrNotExist) && keyErr != nil {
		return nil, nil, keyErr
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("bus: generate CA key: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          randomSerial(),
		Subject:               pkix.Name{CommonName: "DOGGIE bus CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(caValidity),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("bus: create CA certificate: %w", err)
	}
	if err := writePEM(files.CA, "CERTIFICATE", der, 0o644); err != nil {
		return nil, nil, err
	}
	if err := writeKey(files.CAKey, key); err != nil {
		return nil, nil, err
	}
	cert, err = x509.ParseCertificate(der)
	return cert, key, err
}

func serverCertValid(path string, ca *x509.Certificate, hosts []string, now time.Time) bool {
	cert, err := readCert(path)
	if err != nil || now.Add(renewBefore).After(cert.NotAfter) {
		return false
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	for _, h := range hosts {
		opts := x509.VerifyOptions{Roots: roots, DNSName: h, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		if _, err := cert.Verify(opts); err != nil {
			return false
		}
	}
	return true
}

func issueServerCert(files TLSFiles, ca *x509.Certificate, caKey *ecdsa.PrivateKey, hosts []string, now time.Time) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("bus: generate server key: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: randomSerial(),
		Subject:      pkix.Name{CommonName: "DOGGIE bus"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(serverValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return fmt.Errorf("bus: create server certificate: %w", err)
	}
	if err := writeKey(files.Key, key); err != nil {
		return err
	}
	return writePEM(files.Cert, "CERTIFICATE", der, 0o644)
}

func randomSerial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	return n
}

func readCert(path string) (*x509.Certificate, error) {
	block, err := readPEM(path)
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(block.Bytes)
}

func readKey(path string) (*ecdsa.PrivateKey, error) {
	block, err := readPEM(path)
	if err != nil {
		return nil, err
	}
	return x509.ParseECPrivateKey(block.Bytes)
}

func readPEM(path string) (*pem.Block, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("bus: %s is not a PEM file", path)
	}
	return block, nil
}

func writeKey(path string, key *ecdsa.PrivateKey) error {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("bus: encode key: %w", err)
	}
	return writePEM(path, "EC PRIVATE KEY", der, 0o600)
}

// writePEM replaces path at once, so a crash never leaves half a file.
func writePEM(path, typ string, der []byte, perm os.FileMode) error {
	data := pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return fmt.Errorf("bus: write %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("bus: write %s: %w", path, err)
	}
	return nil
}
