package fleet

import (
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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

func TestDeploy(t *testing.T) {
	dir := t.TempDir()
	writePKI(t, dir)
	etcd := startEtcd(t)
	ca, err := LoadCA(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key"), etcd)
	if err != nil {
		t.Fatal(err)
	}
	serverCert, err := NewServerCertificate(ca, []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	fleetServer := NewServer(ca, etcd, "server", newLease(t, etcd))
	srv := NewGRPCServer(ServerCredentials(ca, serverCert), fleetServer)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(lis)
	defer srv.Stop()

	ctx := context.Background()
	// writePKI's agent certificate expires within seconds; this one outlives the test.
	issue(t, dir, "agent-2", &x509.Certificate{
		Subject:     pkix.Name{CommonName: "agent-2"},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}, ca.cert, ca.key.(*ecdsa.PrivateKey))
	agentPEM, err := os.ReadFile(filepath.Join(dir, "agent-2.crt"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := etcd.Put(ctx, signedPrefix+"agent-2", string(agentPEM)); err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := ca.IssueDeployer(ctx, "ci")
	if err != nil {
		t.Fatal(err)
	}
	ciFile := filepath.Join(dir, "ci.pem")
	if err := os.WriteFile(ciFile, append(certPEM, keyPEM...), 0o600); err != nil {
		t.Fatal(err)
	}

	type result struct {
		phases []string
		err    error
	}
	deploy := func(certFile, keyFile, certname, path string, wait bool, timeout time.Duration) result {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		var r result
		r.err = RequestDeploy(ctx, []string{lis.Addr().String()}, filepath.Join(dir, "ca.crt"), certFile, keyFile, certname, path, wait,
			func(p *api.DeployProgress) { r.phases = append(r.phases, p.Phase.String()) })
		return r
	}
	ciDeploy := func(path string, wait bool, timeout time.Duration) <-chan result {
		done := make(chan result, 1)
		go func() { done <- deploy(ciFile, ciFile, "agent-2", path, wait, timeout) }()
		return done
	}
	desired := func() string {
		resp, err := etcd.Get(ctx, agentsPrefix+"agent-2/desired")
		if err != nil || len(resp.Kvs) == 0 {
			return ""
		}
		return string(resp.Kvs[0].Value)
	}
	waitForDesired := func(path string) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); desired() != path; time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("desired = %q, want %q", desired(), path)
			}
		}
	}
	report := func(path string, phase api.DeployProgress_Phase, message string) {
		t.Helper()
		if err := fleetServer.storeDeployProgress(ctx, "agent-2", &api.DeployProgress{StorePath: path, Phase: phase, Message: message}); err != nil {
			t.Fatal(err)
		}
	}
	first, second := storePath('a', "nixos-system-agent-2"), storePath('b', "nixos-system-agent-2")

	if r := <-ciDeploy(first, false, 5*time.Second); r.err != nil || len(r.phases) != 0 {
		t.Fatalf("deploy without wait: %v, phases %v", r.err, r.phases)
	}
	if got := desired(); got != first {
		t.Fatalf("desired = %q, want %q", got, first)
	}

	for _, tc := range []struct {
		name, certFile, keyFile, certname, path string
		code                                    codes.Code
	}{
		{"agent certificate", filepath.Join(dir, "agent-2.crt"), filepath.Join(dir, "agent-2.key"), "agent-2", second, codes.PermissionDenied},
		{"unknown agent", ciFile, ciFile, "agent-9", second, codes.NotFound},
		{"invalid certname", ciFile, ciFile, "../agent-2", second, codes.InvalidArgument},
		{"invalid store path", ciFile, ciFile, "agent-2", "/nix/store/x; reboot", codes.InvalidArgument},
	} {
		if r := deploy(tc.certFile, tc.keyFile, tc.certname, tc.path, false, 5*time.Second); status.Code(r.err) != tc.code {
			t.Errorf("%s: %v, want %v", tc.name, r.err, tc.code)
		}
	}
	if got := desired(); got != first {
		t.Fatalf("rejected requests changed desired to %q", got)
	}

	report(second, api.DeployProgress_PHASE_DONE, "from an earlier deployment")
	done := ciDeploy(second, true, 10*time.Second)
	waitForDesired(second)
	report(first, api.DeployProgress_PHASE_DONE, "")
	report(second, api.DeployProgress_PHASE_ACTIVATING, "")
	report(second, api.DeployProgress_PHASE_DONE, "")
	if r := <-done; r.err != nil || !slices.Equal(r.phases, []string{"PHASE_ACTIVATING", "PHASE_DONE"}) {
		t.Errorf("wait for done: %v, phases %v", r.err, r.phases)
	}

	done = ciDeploy(first, true, 10*time.Second)
	waitForDesired(first)
	report(first, api.DeployProgress_PHASE_FAILED, "switch failed")
	if r := <-done; r.err == nil || !strings.Contains(r.err.Error(), "switch failed") {
		t.Errorf("wait for failure: %v", r.err)
	}

	if r := <-ciDeploy(second, true, 300*time.Millisecond); status.Code(r.err) != codes.DeadlineExceeded {
		t.Errorf("wait without progress: %v, want DeadlineExceeded", r.err)
	}
	if got := desired(); got != second {
		t.Errorf("after the timeout desired = %q, want %q", got, second)
	}
}
