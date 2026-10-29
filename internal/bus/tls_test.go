package bus

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"os"
	"slices"
	"sync"
	"testing"
	"time"
)

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func verifyHosts(t *testing.T, files TLSFiles, hosts []string, now time.Time) {
	t.Helper()
	ca, err := readCert(files.CA)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := readCert(files.Cert)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	for _, h := range hosts {
		if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, DNSName: h, CurrentTime: now}); err != nil {
			t.Errorf("certificate does not verify for %q: %v", h, err)
		}
	}
}

func TestEnsureTLSCreatesFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	hosts := []string{"localhost", "127.0.0.1", "laptop.local", "192.168.1.20"}

	files, err := EnsureTLS(dir, hosts, now)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{files.CA: 0o644, files.CAKey: 0o600, files.Cert: 0o644, files.Key: 0o600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", path, got, want)
		}
	}
	verifyHosts(t, files, hosts, now)
}

func TestEnsureTLSReusesValidCertificates(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	hosts := []string{"localhost"}
	files, _ := EnsureTLS(dir, hosts, now)
	ca, cert := readFile(t, files.CA), readFile(t, files.Cert)

	if _, err := EnsureTLS(dir, hosts, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ca, readFile(t, files.CA)) || !bytes.Equal(cert, readFile(t, files.Cert)) {
		t.Error("certificates changed although they were still valid")
	}
}

func TestEnsureTLSReissuesServerCertKeepingCA(t *testing.T) {
	tests := []struct {
		name  string
		hosts []string
		now   time.Time
	}{
		{name: "new host", hosts: []string{"localhost", "10.0.0.5"}, now: time.Now()},
		{name: "near expiry", hosts: []string{"localhost"}, now: time.Now().Add(serverValidity - renewBefore/2)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			files, _ := EnsureTLS(dir, []string{"localhost"}, time.Now())
			ca, cert := readFile(t, files.CA), readFile(t, files.Cert)

			if _, err := EnsureTLS(dir, tt.hosts, tt.now); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(ca, readFile(t, files.CA)) {
				t.Error("CA changed: satellites would stop trusting the bus")
			}
			if bytes.Equal(cert, readFile(t, files.Cert)) {
				t.Error("server certificate was not reissued")
			}
			verifyHosts(t, files, tt.hosts, tt.now)
		})
	}
}

func TestTLSHosts(t *testing.T) {
	hosts := TLSHosts("192.168.1.20:4222", []string{"doggie.tailnet.ts.net"})
	for _, want := range []string{"localhost", "127.0.0.1", "192.168.1.20", "doggie.tailnet.ts.net"} {
		if !slices.Contains(hosts, want) {
			t.Errorf("hosts %v missing %q", hosts, want)
		}
	}
	if hosts := TLSHosts("0.0.0.0:4222", nil); slices.Contains(hosts, "0.0.0.0") {
		t.Errorf("hosts %v include the unspecified address", hosts)
	}
}

func TestTLSServerRequiresTrustedCA(t *testing.T) {
	dir := t.TempDir()
	tlsCfg, err := ServerTLS(dir, func() []string { return []string{"localhost", "127.0.0.1"} }, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	files := TLSFilesIn(dir)
	srv, err := Start(context.Background(), Options{
		Listen: "127.0.0.1:-1", Token: "t", StoreDir: t.TempDir(),
		MaxAge: time.Hour, MaxBytes: 1 << 20, TLS: tlsCfg, Log: discardLog(),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Shutdown()
	url := srv.ClientURL()

	if _, err := Dial(url, DialOptions{Name: "no-ca", Token: "t", Log: discardLog()}); err == nil {
		t.Error("dial without the CA succeeded")
	}
	otherCA, _ := EnsureTLS(t.TempDir(), []string{"localhost"}, time.Now())
	if _, err := Dial(url, DialOptions{Name: "wrong-ca", Token: "t", CAFile: otherCA.CA, Log: discardLog()}); err == nil {
		t.Error("dial trusting another CA succeeded")
	}
	c, err := Dial(url, DialOptions{Name: "satellite", Token: "t", CAFile: files.CA, Log: discardLog()})
	if err != nil {
		t.Fatalf("dial with the bus CA: %v", err)
	}
	c.Close()

	inproc, err := srv.Connect("component")
	if err != nil {
		t.Fatalf("in-process connect with TLS enabled: %v", err)
	}
	inproc.Close()
}

func TestEnsureTLSRejectsCorruptFiles(t *testing.T) {
	dir := t.TempDir()
	files := TLSFilesIn(dir)
	if err := os.WriteFile(files.CA, []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(files.CAKey, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureTLS(dir, []string{"localhost"}, time.Now()); err == nil {
		t.Fatal("EnsureTLS accepted a corrupt CA instead of failing")
	}
}

// tlsServer starts a TLS bus whose certificate covers what hosts returns
// at each moment.
func tlsServer(t *testing.T, dir string, hosts func() []string) *Server {
	t.Helper()
	tlsCfg, err := ServerTLS(dir, hosts, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	srv, err := Start(context.Background(), Options{
		Listen: "127.0.0.1:-1", Token: "t", StoreDir: t.TempDir(),
		MaxAge: time.Hour, MaxBytes: 1 << 20, TLS: tlsCfg, Log: discardLog(),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Shutdown)
	return srv
}

func dialTLS(dir, url string) error {
	c, err := Dial(url, DialOptions{Name: "satellite", Token: "t", CAFile: TLSFilesIn(dir).CA, Log: discardLog()})
	if err == nil {
		c.Close()
	}
	return err
}

// An address that comes up after DOGGIE started, as Tailscale's may, is
// covered as soon as a satellite reaches the server through it.
func TestServerCertCoversAnAddressThatCameLater(t *testing.T) {
	dir := t.TempDir()
	var mu sync.Mutex
	hosts := []string{"localhost"}
	srv := tlsServer(t, dir, func() []string { mu.Lock(); defer mu.Unlock(); return slices.Clone(hosts) })
	url := srv.ClientURL() // by IP: 127.0.0.1
	if err := dialTLS(dir, url); err == nil {
		t.Fatal("the certificate covered 127.0.0.1 before it was listed")
	}

	mu.Lock()
	hosts = append(hosts, "127.0.0.1")
	mu.Unlock()
	if err := dialTLS(dir, url); err != nil {
		t.Fatalf("dial by the new address: %v", err)
	}
	verifyHosts(t, TLSFilesIn(dir), []string{"localhost", "127.0.0.1"}, time.Now())
}

// A name DOGGIE doesn't list is never added, however many clients ask.
func TestServerCertIgnoresNamesItDoesNotList(t *testing.T) {
	dir := t.TempDir()
	srv := tlsServer(t, dir, func() []string { return []string{"localhost"} })
	before := readFile(t, TLSFilesIn(dir).Cert)
	for range 3 {
		if err := dialTLS(dir, srv.ClientURL()); err == nil {
			t.Fatal("dial by an address the certificate doesn't cover succeeded")
		}
	}
	if !bytes.Equal(before, readFile(t, TLSFilesIn(dir).Cert)) {
		t.Error("the certificate was reissued for an address DOGGIE doesn't list")
	}
}

// A certificate that nears expiry while DOGGIE runs is renewed on the next
// handshake.
func TestServerCertRenewsWhileRunning(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	s := &certSource{dir: dir, hosts: func() []string { return []string{"localhost"} }, log: discardLog(), now: func() time.Time { return now }}
	if err := s.load(s.hosts()); err != nil {
		t.Fatal(err)
	}
	first := s.cert.Leaf.NotAfter
	hello := &tls.ClientHelloInfo{ServerName: "localhost"}
	if c, _ := s.get(hello); !c.Leaf.NotAfter.Equal(first) {
		t.Fatal("a fresh certificate was reissued")
	}

	now = first.Add(-renewBefore + time.Hour)
	c, err := s.get(hello)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Leaf.NotAfter.After(first) {
		t.Errorf("certificate still ends %s, want it renewed", c.Leaf.NotAfter)
	}
}

// A key that doesn't match its certificate, as a crash between writing
// them leaves, doesn't keep DOGGIE from starting.
func TestServerTLSReplacesAMismatchedKey(t *testing.T) {
	dir := t.TempDir()
	hosts := []string{"localhost"}
	if _, err := EnsureTLS(dir, hosts, time.Now()); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	if _, err := EnsureTLS(other, hosts, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(TLSFilesIn(dir).Key, readFile(t, TLSFilesIn(other).Key), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ServerTLS(dir, func() []string { return hosts }, discardLog()); err != nil {
		t.Fatalf("ServerTLS with a mismatched key: %v", err)
	}
}
