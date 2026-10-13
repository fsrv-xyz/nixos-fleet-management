package fleet

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/peer"

	"nixos-fleet-management/api"
)

type peerRecordingServer struct {
	*Server
	peers chan net.Addr
}

func (s *peerRecordingServer) Connect(stream grpc.BidiStreamingServer[api.AgentMessage, api.ServerMessage]) error {
	p, _ := peer.FromContext(stream.Context())
	s.peers <- p.Addr
	return s.Server.Connect(stream)
}

func TestAgentMovesFromIPv4ToIPv6(t *testing.T) {
	recheck := ipv6RecheckInterval
	ipv6RecheckInterval = 100 * time.Millisecond
	t.Cleanup(func() { ipv6RecheckInterval = recheck })

	dir := t.TempDir()
	writePKI(t, dir)
	etcd := startEtcd(t)
	ca, err := LoadCA(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key"), etcd)
	if err != nil {
		t.Fatal(err)
	}
	certFile := filepath.Join(dir, "agent.crt")
	agentCert, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := etcd.Put(context.Background(), signedPrefix+"agent-1", string(agentCert)); err != nil {
		t.Fatal(err)
	}
	serverCert, err := NewServerCertificate(ca, []string{"localhost"})
	if err != nil {
		t.Fatal(err)
	}

	lis4, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(lis4.Addr().(*net.TCPAddr).Port)
	peers := make(chan net.Addr, 10)
	s := grpc.NewServer(grpc.Creds(ServerCredentials(ca, serverCert)))
	api.RegisterFleetServer(s, &peerRecordingServer{Server: NewServer(ca, etcd, "server", newLease(t, etcd)), peers: peers})
	defer s.Stop()
	go s.Serve(lis4)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go RunAgent(ctx, []string{net.JoinHostPort("localhost", port)}, filepath.Join(dir, "ca.crt"), certFile, filepath.Join(dir, "agent.key"), 100*time.Millisecond)
	nextPeer := func() *net.TCPAddr {
		t.Helper()
		select {
		case addr := <-peers:
			return addr.(*net.TCPAddr)
		case <-time.After(maxReconnectDelay + 5*time.Second):
			t.Fatal("agent did not connect")
			return nil
		}
	}
	if addr := nextPeer(); addr.IP.To4() == nil {
		t.Fatalf("agent connected from %s, want IPv4 while the server listens on IPv4 only", addr)
	}

	lis6, err := net.Listen("tcp6", net.JoinHostPort("::1", port))
	if err != nil {
		t.Skipf("no IPv6 loopback: %v", err)
	}
	go s.Serve(lis6)
	if addr := nextPeer(); addr.IP.To4() != nil {
		t.Fatalf("agent reconnected from %s, want IPv6", addr)
	}
}

func TestIsClosed(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	conn, err := net.Dial("tcp", lis.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	tcp := conn.(*net.TCPConn)
	if isClosed(tcp) {
		t.Fatal("open connection reported as closed")
	}
	conn.Close()
	if !isClosed(tcp) {
		t.Fatal("closed connection reported as open")
	}
}
