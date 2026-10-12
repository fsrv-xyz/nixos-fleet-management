package fleet

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"path/filepath"
	"testing"
	"time"
)

func testCA(t *testing.T) *CA {
	t.Helper()
	dir := t.TempDir()
	writePKI(t, dir)
	ca, err := LoadCA(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key"), nil)
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

func TestServerCertificateRoles(t *testing.T) {
	ca := testCA(t)
	roots := x509.NewCertPool()
	roots.AddCert(ca.cert)
	verify := func(cert *x509.Certificate, name string, usage x509.ExtKeyUsage) error {
		_, err := cert.Verify(x509.VerifyOptions{DNSName: name, Roots: roots, KeyUsages: []x509.ExtKeyUsage{usage}})
		return err
	}

	server, err := NewServerCertificate(ca, []string{"fleet1.example.org", "10.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := server.get(nil)
	if _, ok := cert.PrivateKey.(ed25519.PrivateKey); !ok {
		t.Errorf("private key is %T", cert.PrivateKey)
	}
	for _, name := range []string{"fleet1.example.org", "10.0.0.1"} {
		if err := verify(cert.Leaf, name, x509.ExtKeyUsageServerAuth); err != nil {
			t.Errorf("server certificate for %s: %v", name, err)
		}
	}
	if verify(cert.Leaf, "fleet2.example.org", x509.ExtKeyUsageServerAuth) == nil {
		t.Error("server certificate valid for a name it was not issued for")
	}
	if verify(cert.Leaf, "", x509.ExtKeyUsageClientAuth) == nil {
		t.Error("server certificate usable to log in as an agent")
	}

	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	agentPEM, err := ca.issue("fleet1.example.org", public)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := parseCertificate(agentPEM)
	if err != nil {
		t.Fatal(err)
	}
	if verify(agent, "fleet1.example.org", x509.ExtKeyUsageServerAuth) == nil {
		t.Error("agent certificate usable to impersonate a server")
	}

	if _, err := NewServerCertificate(ca, nil); err == nil {
		t.Error("server certificate issued without names")
	}
}

func TestServerCertificateRenews(t *testing.T) {
	server, err := newServerCertificate(testCA(t), []string{"127.0.0.1"}, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := server.get(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go server.Run(ctx)

	for {
		current, _ := server.get(nil)
		if current.Leaf.SerialNumber.Cmp(first.Leaf.SerialNumber) != 0 {
			if !current.Leaf.NotAfter.After(first.Leaf.NotAfter) {
				t.Errorf("renewed certificate expires %s, not after %s", current.Leaf.NotAfter, first.Leaf.NotAfter)
			}
			return
		}
		if ctx.Err() != nil {
			t.Fatal("certificate not renewed after two thirds of its validity")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
