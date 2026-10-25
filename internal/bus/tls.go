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
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// ServerTLSConfig loads the server certificate for the NATS server.
func ServerTLSConfig(files TLSFiles) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(files.Cert, files.Key)
	if err != nil {
		return nil, fmt.Errorf("bus: load server certificate: %w", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}, nil
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

func writePEM(path, typ string, der []byte, perm os.FileMode) error {
	data := pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
	if err := os.WriteFile(path, data, perm); err != nil {
		return fmt.Errorf("bus: write %s: %w", path, err)
	}
	return nil
}
