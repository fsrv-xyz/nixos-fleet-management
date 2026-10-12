package fleet

import (
	"context"
	"crypto/tls"
	"log"
	"os"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/resolver/manual"
	"google.golang.org/protobuf/proto"

	"nixos-fleet-management/api"
)

const (
	maxReconnectDelay = 10 * time.Second
	renewRetryDelay   = time.Minute
)

func RunAgent(ctx context.Context, addrs []string, caFile, certFile, keyFile string, interval time.Duration) error {
	pool, err := loadPool(caFile)
	if err != nil {
		return err
	}
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return err
	}
	var cert atomic.Pointer[tls.Certificate]
	cert.Store(&pair)
	creds := credentials.NewTLS(&tls.Config{
		MinVersion: tls.VersionTLS13,
		RootCAs:    pool,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return cert.Load(), nil
		},
	})

	bo := backoff.DefaultConfig
	bo.MaxDelay = maxReconnectDelay
	conn, err := dial(addrs, creds,
		grpc.WithConnectParams(grpc.ConnectParams{Backoff: bo, MinConnectTimeout: 5 * time.Second}),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 30 * time.Second, Timeout: 10 * time.Second, PermitWithoutStream: true}),
	)
	if err != nil {
		return err
	}
	defer conn.Close()
	client := api.NewFleetClient(conn)

	go renewLoop(ctx, client, &cert, certFile, keyFile)

	// Progress outlives sessions, so the final state of a deployment that ended
	// while disconnected is reported after the reconnect.
	reports := make(latest[*api.DeployProgress], 1)
	deployer := newDeployer(reports.offer)
	go deployer.run(ctx)

	for {
		err := session(ctx, client, deployer, reports, interval)
		log.Printf("stream ended: %v", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		// Rate-limits retries when the connection is up but the stream itself fails.
		case <-time.After(time.Second):
		}
	}
}

// dial connects to one of addrs and fails over to the others; the shuffle spreads agents across servers.
func dial(addrs []string, creds credentials.TransportCredentials, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	endpoints := make([]resolver.Endpoint, len(addrs))
	for i, addr := range addrs {
		endpoints[i] = resolver.Endpoint{Addresses: []resolver.Address{{Addr: addr, ServerName: addr}}}
	}
	r := manual.NewBuilderWithScheme("fleet")
	r.InitialState(resolver.State{Endpoints: endpoints})
	return grpc.NewClient(r.Scheme()+":///servers", append(opts,
		grpc.WithResolvers(r),
		grpc.WithTransportCredentials(creds),
		grpc.WithDefaultServiceConfig(`{"loadBalancingConfig": [{"pick_first": {"shuffleAddressList": true}}]}`),
	)...)
}

// renewLoop renews the certificate after two thirds of its lifetime. Established
// connections keep the old certificate, new ones present the renewed one.
func renewLoop(ctx context.Context, client api.FleetClient, cert *atomic.Pointer[tls.Certificate], certFile, keyFile string) {
	for {
		leaf := cert.Load().Leaf
		renewAt := leaf.NotBefore.Add(leaf.NotAfter.Sub(leaf.NotBefore) * 2 / 3)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(renewAt)):
		}

		renewed, err := renew(ctx, client, certFile, keyFile)
		if err != nil {
			log.Printf("certificate renewal failed: %v", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(renewRetryDelay):
			}
			continue
		}
		cert.Store(renewed)
		log.Printf("certificate renewed, valid until %s", renewed.Leaf.NotAfter)
	}
}

func renew(ctx context.Context, client api.FleetClient, certFile, keyFile string) (*tls.Certificate, error) {
	resp, err := client.RenewCertificate(ctx, &api.RenewCertificateRequest{}, grpc.WaitForReady(true))
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, err
	}
	pair, err := tls.X509KeyPair(resp.Certificate, keyPEM)
	if err != nil {
		return nil, err
	}
	// Replaced atomically: a truncated certificate file would lock the agent out.
	tmp := certFile + ".tmp"
	if err := os.WriteFile(tmp, resp.Certificate, 0o644); err != nil {
		return nil, err
	}
	return &pair, os.Rename(tmp, certFile)
}

func session(ctx context.Context, client api.FleetClient, deployer *deployer, reports latest[*api.DeployProgress], interval time.Duration) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := client.Connect(ctx, grpc.WaitForReady(true))
	if err != nil {
		return err
	}
	recvErr := make(chan error, 1)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			if deploy := msg.GetDeploy(); deploy != nil {
				deployer.offer(deploy.StorePath)
			}
		}
	}()
	// Only this goroutine sends; a failed Send reports the stream error through Recv.
	send := func(msg *api.AgentMessage) error {
		if err := stream.Send(msg); err != nil {
			return <-recvErr
		}
		return nil
	}

	facts := collectFacts()
	if err := send(&api.AgentMessage{Body: &api.AgentMessage_Facts{Facts: facts}}); err != nil {
		return err
	}
	seq := uint64(1)
	if err := send(heartbeat(seq)); err != nil {
		return err
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-recvErr:
			return err
		case progress := <-reports:
			if err := send(&api.AgentMessage{Body: &api.AgentMessage_DeployProgress{DeployProgress: progress}}); err != nil {
				return err
			}
		case <-ticker.C:
			seq++
			if err := send(heartbeat(seq)); err != nil {
				return err
			}
			// Facts such as the NixOS generation change while the agent runs.
			if current := collectFacts(); !proto.Equal(current, facts) {
				facts = current
				if err := send(&api.AgentMessage{Body: &api.AgentMessage_Facts{Facts: facts}}); err != nil {
					return err
				}
			}
		}
	}
}

func heartbeat(seq uint64) *api.AgentMessage {
	return &api.AgentMessage{Body: &api.AgentMessage_Heartbeat{Heartbeat: &api.Heartbeat{Seq: seq}}}
}
