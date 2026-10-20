package fleet

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"
	"go.etcd.io/etcd/server/v3/embed"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"

	"nixos-fleet-management/api"
)

type notifyingServer struct {
	*Server
	connected chan string
}

func (s *notifyingServer) Connect(stream grpc.BidiStreamingServer[api.AgentMessage, api.ServerMessage]) error {
	id, _ := AgentID(stream.Context())
	s.connected <- id
	return s.Server.Connect(stream)
}

func TestAgentRenewsAndFailsOver(t *testing.T) {
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
	serverCreds := ServerCredentials(ca, serverCert)
	certFile := filepath.Join(dir, "agent.crt")
	agentCert, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := etcd.Put(context.Background(), signedPrefix+"agent-1", string(agentCert)); err != nil {
		t.Fatal(err)
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addrA, addrB := lis.Addr().String(), freeAddr(t)
	connected := make(chan string, 10)
	watchCtx, stopWatches := context.WithCancel(context.Background())
	defer stopWatches()
	serve := func(lis net.Listener, name string) *grpc.Server {
		s := grpc.NewServer(grpc.Creds(serverCreds))
		fleetServer := NewServer(ca, etcd, name, newLease(t, etcd))
		go fleetServer.WatchAgents(watchCtx)
		api.RegisterFleetServer(s, &notifyingServer{Server: fleetServer, connected: connected})
		go s.Serve(lis)
		return s
	}
	expectAgent := func() {
		t.Helper()
		select {
		case id := <-connected:
			if id != "agent-1" {
				t.Fatalf("agent ID = %q, want agent-1", id)
			}
		case <-time.After(maxReconnectDelay + 5*time.Second):
			t.Fatal("agent did not connect")
		}
	}

	desiredBefore, desiredAfter := storePath('a', "nixos-system-agent-1"), storePath('b', "nixos-system-agent-1")
	if _, err := SetDesired(context.Background(), etcd, "agent-1", desiredBefore); err != nil {
		t.Fatal(err)
	}

	srv := serve(lis, "server-a")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go RunAgent(ctx, []string{addrB, addrA}, filepath.Join(dir, "ca.crt"), certFile, filepath.Join(dir, "agent.key"), 100*time.Millisecond)
	expectAgent()

	for {
		data, _ := os.ReadFile(certFile)
		if cert, err := parseCertificate(data); err == nil && time.Until(cert.NotAfter) > agentCertValidity/2 {
			break
		}
		if time.Until(agentCertExpiry(t, agentCert)) < 0 {
			t.Fatal("certificate was not renewed before it expired")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The renewed certificate only proves itself on a new handshake after the old one expired.
	time.Sleep(time.Until(agentCertExpiry(t, agentCert)))

	srv.Stop()
	lis, err = net.Listen("tcp", addrB)
	if err != nil {
		t.Fatal(err)
	}
	srv = serve(lis, "server-b")
	defer srv.Stop()
	expectAgent()

	t.Run("frontend of another server streams agents", func(t *testing.T) {
		observer := NewServer(ca, etcd, "observer", newLease(t, etcd))
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		go observer.WatchAgents(ctx)
		handler := observer.HTTPHandler()
		web := httptest.NewServer(handler)
		defer web.Close()
		req, err := http.NewRequestWithContext(ctx, "GET", web.URL+"/api/agents/events", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		events := bufio.NewScanner(resp.Body)
		// The latest list carries over, as one event may satisfy several waits.
		var agents []AgentStatus
		var last string
		waitFor := func(match func([]AgentStatus) bool) {
			t.Helper()
			if agents != nil && match(agents) {
				return
			}
			for events.Scan() {
				data, ok := strings.CutPrefix(events.Text(), "data: ")
				if !ok {
					continue
				}
				last = data
				var next []AgentStatus
				if err := json.Unmarshal([]byte(data), &next); err != nil {
					t.Fatalf("%v: %s", err, data)
				}
				if agents = next; match(agents) {
					return
				}
			}
			t.Fatalf("event stream ended: %v, last event: %s", events.Err(), last)
		}

		waitFor(func(agents []AgentStatus) bool {
			return len(agents) == 1 && agents[0].ID == "agent-1" && agents[0].Server == "server-b" &&
				strings.Contains(string(agents[0].Facts), `"goos":"`+runtime.GOOS+`"`)
		})
		// Not a NixOS host, so each deployment the agent receives ends as failed.
		deployed := func(path string) func([]AgentStatus) bool {
			return func(agents []AgentStatus) bool {
				var deploy DeployStatus
				return len(agents) == 1 && agents[0].Desired == path &&
					json.Unmarshal(agents[0].Deploy, &deploy) == nil &&
					deploy.StorePath == path && deploy.Phase == "PHASE_FAILED"
			}
		}
		waitFor(deployed(desiredBefore))
		if _, err := SetDesired(ctx, etcd, "agent-1", desiredAfter); err != nil {
			t.Fatal(err)
		}
		waitFor(deployed(desiredAfter))

		if err := observer.putAgent(ctx, agentsPrefix+"agent-2/conns/test", AgentStatus{ID: "agent-2", Server: "observer"}); err != nil {
			t.Fatal(err)
		}
		waitFor(func(agents []AgentStatus) bool {
			return len(agents) == 2 && agents[1].ID == "agent-2"
		})

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "app.js") {
			t.Fatalf("index: %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("client without certificate is rejected", func(t *testing.T) {
		pool := x509.NewCertPool()
		ca, _ := os.ReadFile(filepath.Join(dir, "ca.crt"))
		pool.AppendCertsFromPEM(ca)
		conn, err := grpc.NewClient(addrB, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: pool})))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		stream, err := api.NewFleetClient(conn).Connect(ctx)
		if err == nil {
			_, err = stream.Recv()
		}
		if err == nil {
			t.Fatal("connection without client certificate succeeded")
		}
	})
}

func TestStoreFacts(t *testing.T) {
	etcd := startEtcd(t)
	s := NewServer(nil, etcd, "server", 0)
	ctx := context.Background()
	facts := func(goos string) *api.Facts {
		platform, err := anypb.New(&api.Platform{Goos: goos, Goarch: "arm64"})
		if err != nil {
			t.Fatal(err)
		}
		return &api.Facts{Facts: []*anypb.Any{platform}}
	}
	version := func() int64 {
		resp, err := etcd.Get(ctx, agentsPrefix+"node1/facts")
		if err != nil || len(resp.Kvs) == 0 {
			t.Fatalf("facts not stored: %v", err)
		}
		return resp.Kvs[0].Version
	}

	for _, step := range []struct {
		goos    string
		version int64
	}{{"linux", 1}, {"linux", 1}, {"darwin", 2}} {
		if err := s.storeFacts(ctx, "node1", facts(step.goos)); err != nil {
			t.Fatal(err)
		}
		if v := version(); v != step.version {
			t.Errorf("after storing %s: version %d, want %d", step.goos, v, step.version)
		}
	}

	unknown := &api.Facts{Facts: []*anypb.Any{{TypeUrl: "type.googleapis.com/unknown.Fact"}}}
	if err := s.storeFacts(ctx, "node1", unknown); status.Code(err) != codes.InvalidArgument {
		t.Errorf("unknown fact type: %v", err)
	}
	huge := &api.Facts{Facts: []*anypb.Any{{TypeUrl: "type.googleapis.com/fleet.v1.Platform", Value: make([]byte, maxFactsSize)}}}
	if err := s.storeFacts(ctx, "node1", huge); status.Code(err) != codes.InvalidArgument {
		t.Errorf("oversized facts: %v", err)
	}
}

func TestAgentFeedSendsEmptyListAsArray(t *testing.T) {
	s := NewServer(nil, nil, "", 0)
	if err := s.publish(map[string]*agentState{}); err != nil {
		t.Fatal(err)
	}
	if agents, _ := s.feed.get(); string(agents) != "[]" {
		t.Fatalf("empty agent list published as %s", agents)
	}
}

func startEtcd(t *testing.T) *clientv3.Client {
	t.Helper()
	cfg := embed.NewConfig()
	cfg.Dir = t.TempDir()
	cfg.LogLevel = "error"
	client, _ := url.Parse("http://127.0.0.1:0")
	peer, _ := url.Parse("http://" + freeAddr(t))
	cfg.ListenClientUrls, cfg.AdvertiseClientUrls = []url.URL{*client}, []url.URL{*client}
	cfg.ListenPeerUrls, cfg.AdvertisePeerUrls = []url.URL{*peer}, []url.URL{*peer}
	cfg.InitialCluster = cfg.InitialClusterFromName(cfg.Name)
	e, err := embed.StartEtcd(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	select {
	case <-e.Server.ReadyNotify():
	case <-time.After(10 * time.Second):
		t.Fatal("etcd did not start")
	}
	c, err := clientv3.New(clientv3.Config{Endpoints: []string{e.Clients[0].Addr().String()}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func freeAddr(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	return lis.Addr().String()
}

func newLease(t *testing.T, etcd *clientv3.Client) clientv3.LeaseID {
	t.Helper()
	session, err := concurrency.NewSession(etcd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session.Lease()
}

func agentCertExpiry(t *testing.T, certPEM []byte) time.Time {
	t.Helper()
	cert, err := parseCertificate(certPEM)
	if err != nil {
		t.Fatal(err)
	}
	return cert.NotAfter
}

func TestCertificateSigning(t *testing.T) {
	dir := t.TempDir()
	writePKI(t, dir)
	ca, err := LoadCA(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key"), startEtcd(t))
	if err != nil {
		t.Fatal(err)
	}
	serverCert, err := NewServerCertificate(ca, []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	creds := CAServerCredentials(serverCert)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.Creds(creds))
	api.RegisterCertificateAuthorityServer(srv, ca)
	go srv.Serve(lis)
	defer srv.Stop()

	agentDir := t.TempDir()
	certFile, keyFile := filepath.Join(agentDir, "agent.crt"), filepath.Join(agentDir, "agent.key")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- EnsureCertificate(ctx, []string{freeAddr(t), lis.Addr().String()}, filepath.Join(dir, "ca.crt"), certFile, keyFile, "node1", 50*time.Millisecond)
	}()

	for {
		var pending bytes.Buffer
		if err := ca.List(ctx, &pending); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(pending.String(), "node1 SHA256:") {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("returned before signing: %v", err)
		case <-ctx.Done():
			t.Fatal("no pending request")
		case <-time.After(10 * time.Millisecond):
		}
	}

	for name, want := range map[string]codes.Code{"node1": codes.AlreadyExists, "../node1": codes.InvalidArgument} {
		_, err := ca.RequestCertificate(ctx, &api.CertificateRequest{Csr: rogueCSR(t, name)})
		if status.Code(err) != want {
			t.Errorf("request for %q: %v, want %v", name, err, want)
		}
	}

	if err := ca.Sign(ctx, "node1"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := tls.LoadX509KeyPair(certFile, keyFile); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(certFile)
	cert, err := parseCertificate(data)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.cert)
	if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatal(err)
	}
	if _, err := ca.RequestCertificate(ctx, &api.CertificateRequest{Csr: rogueCSR(t, "node1")}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("request for signed name with other key: %v", err)
	}
	rogueKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ca.Renew(ctx, "node1", rogueKey.Public()); status.Code(err) != codes.PermissionDenied {
		t.Errorf("renewal with other key: %v", err)
	}
}

func rogueCSR(t *testing.T, name string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := newCSR(key, name)
	if err != nil {
		t.Fatal(err)
	}
	return csr
}

func writePKI(t *testing.T, dir string) {
	t.Helper()
	caKey, caCert := issue(t, dir, "ca", &x509.Certificate{
		Subject:               pkix.Name{CommonName: "test-ca"},
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}, nil, nil)
	issue(t, dir, "agent", &x509.Certificate{
		Subject:     pkix.Name{CommonName: "agent-1"},
		NotAfter:    time.Now().Add(3 * time.Second),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}, caCert, caKey)
}

func issue(t *testing.T, dir, name string, tmpl, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) (*ecdsa.PrivateKey, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl.SerialNumber = big.NewInt(time.Now().UnixNano())
	tmpl.NotBefore = time.Now().Add(-time.Minute)
	if tmpl.NotAfter.IsZero() {
		tmpl.NotAfter = time.Now().Add(time.Hour)
	}
	if parent == nil {
		parent, parentKey = tmpl, key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, parentKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	writePEM(t, filepath.Join(dir, name+".crt"), "CERTIFICATE", der)
	writePEM(t, filepath.Join(dir, name+".key"), "EC PRIVATE KEY", keyDER)
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return key, cert
}

func writePEM(t *testing.T, path, typ string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}
