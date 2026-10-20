package fleet

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"nixos-fleet-management/api"
)

func tlsPeer(cert *x509.Certificate) context.Context {
	return peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{
		State: tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{cert}}},
	}})
}

func TestDeployCertificates(t *testing.T) {
	dir := t.TempDir()
	writePKI(t, dir)
	etcd := startEtcd(t)
	ca, err := LoadCA(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key"), etcd)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	parse := func(certPEM []byte) *x509.Certificate {
		t.Helper()
		cert, err := parseCertificate(certPEM)
		if err != nil {
			t.Fatal(err)
		}
		return cert
	}

	certPEM, keyPEM, err := ca.IssueDeployer(ctx, "ci")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		t.Fatal(err)
	}
	cert := parse(certPEM)
	roots := x509.NewCertPool()
	roots.AddCert(ca.cert)
	if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatal(err)
	}
	if !isDeployer(cert) {
		t.Fatalf("issued certificate has OU %v", cert.Subject.OrganizationalUnit)
	}
	if err := ca.authorizeDeployer(ctx, cert); err != nil {
		t.Fatalf("issued certificate: %v", err)
	}

	if _, err := AgentID(tlsPeer(cert)); status.Code(err) != codes.PermissionDenied {
		t.Errorf("deploy certificate as agent: %v", err)
	}
	s := NewServer(ca, etcd, "server", 0)
	if _, err := s.RenewCertificate(tlsPeer(cert), &api.RenewCertificateRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("renewing a deploy certificate: %v", err)
	}

	agentPEM, err := os.ReadFile(filepath.Join(dir, "agent.crt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ca.authorizeDeployer(ctx, parse(agentPEM)); status.Code(err) != codes.PermissionDenied {
		t.Errorf("agent certificate as deployer: %v", err)
	}

	reissuedPEM, _, err := ca.IssueDeployer(ctx, "ci")
	if err != nil {
		t.Fatal(err)
	}
	if err := ca.authorizeDeployer(ctx, cert); status.Code(err) != codes.PermissionDenied {
		t.Errorf("replaced certificate: %v", err)
	}
	reissued := parse(reissuedPEM)
	if err := ca.authorizeDeployer(ctx, reissued); err != nil {
		t.Errorf("reissued certificate: %v", err)
	}
	if err := ca.RevokeDeployer(ctx, "ci"); err != nil {
		t.Fatal(err)
	}
	if err := ca.authorizeDeployer(ctx, reissued); status.Code(err) != codes.PermissionDenied {
		t.Errorf("revoked certificate: %v", err)
	}
	if err := ca.RevokeDeployer(ctx, "ci"); err == nil {
		t.Error("revoking an absent certificate succeeded")
	}
	if _, _, err := ca.IssueDeployer(ctx, "../ci"); err == nil {
		t.Error("issued a certificate for an invalid name")
	}
}
