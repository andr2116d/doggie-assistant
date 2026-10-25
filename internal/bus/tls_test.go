package bus

import (
	"bytes"
	"context"
	"crypto/x509"
	"os"
	"slices"
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
	files, err := EnsureTLS(t.TempDir(), []string{"localhost", "127.0.0.1"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg, err := ServerTLSConfig(files)
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
