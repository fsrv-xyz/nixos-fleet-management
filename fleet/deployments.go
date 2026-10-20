package fleet

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"

	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"nixos-fleet-management/api"
)

type Deployments struct {
	api.UnimplementedDeploymentsServer
	ca   *CA
	etcd *clientv3.Client
}

func (d *Deployments) Deploy(req *api.DeployRequest, stream grpc.ServerStreamingServer[api.DeployProgress]) error {
	ctx := stream.Context()
	cert, err := clientCertificate(ctx)
	if err != nil {
		return err
	}
	if err := d.ca.authorizeDeployer(ctx, cert); err != nil {
		return err
	}
	if err := validateDeploy(req.Certname, req.StorePath); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	signed, err := d.etcd.Get(ctx, signedPrefix+req.Certname, clientv3.WithCountOnly())
	if err != nil {
		return status.Errorf(codes.Unavailable, "look up %s: %v", req.Certname, err)
	}
	if signed.Count == 0 {
		return status.Errorf(codes.NotFound, "no agent %s", req.Certname)
	}
	revision, err := SetDesired(ctx, d.etcd, req.Certname, req.StorePath)
	if err != nil {
		return status.Errorf(codes.Unavailable, "set desired state: %v", err)
	}
	log.Printf("deploy %s %s by %s", req.Certname, req.StorePath, cert.Subject.CommonName)
	if !req.Wait {
		return nil
	}
	return d.follow(ctx, stream, req.Certname, req.StorePath, revision)
}

// follow watches from just after the desired change, so progress left from an
// earlier deployment of the same store path does not count.
func (d *Deployments) follow(ctx context.Context, stream grpc.ServerStreamingServer[api.DeployProgress], certname, storePath string, revision int64) error {
	for wr := range d.etcd.Watch(ctx, agentsPrefix+certname+"/deploy", clientv3.WithRev(revision+1)) {
		if err := wr.Err(); err != nil {
			return status.Errorf(codes.Unavailable, "watch progress: %v", err)
		}
		for _, ev := range wr.Events {
			if ev.Type == clientv3.EventTypeDelete {
				continue
			}
			var deploy DeployStatus
			if err := json.Unmarshal(ev.Kv.Value, &deploy); err != nil {
				return err
			}
			if deploy.StorePath != storePath {
				continue
			}
			phase := api.DeployProgress_Phase(api.DeployProgress_Phase_value[deploy.Phase])
			if err := stream.Send(&api.DeployProgress{StorePath: storePath, Phase: phase, Message: deploy.Message}); err != nil {
				return err
			}
			if phase == api.DeployProgress_PHASE_DONE || phase == api.DeployProgress_PHASE_FAILED {
				return nil
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return status.FromContextError(err).Err()
	}
	return status.Error(codes.Unavailable, "progress watch closed")
}

// RequestDeploy assigns storePath to certname through one of addrs and, with
// wait, reports progress until the deployment ends; ctx bounds both.
func RequestDeploy(ctx context.Context, addrs []string, caFile, certFile, keyFile, certname, storePath string, wait bool, report func(*api.DeployProgress)) error {
	pool, err := loadPool(caFile)
	if err != nil {
		return err
	}
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return err
	}
	conn, err := dial(addrs, credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, Certificates: []tls.Certificate{pair}}))
	if err != nil {
		return err
	}
	defer conn.Close()
	stream, err := api.NewDeploymentsClient(conn).Deploy(ctx, &api.DeployRequest{Certname: certname, StorePath: storePath, Wait: wait}, grpc.WaitForReady(true))
	if err != nil {
		return err
	}
	for {
		progress, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			if wait {
				return errors.New("server ended the stream before the deployment finished")
			}
			return nil
		}
		if err != nil {
			return err
		}
		report(progress)
		switch progress.Phase {
		case api.DeployProgress_PHASE_DONE:
			return nil
		case api.DeployProgress_PHASE_FAILED:
			return fmt.Errorf("deployment of %s failed: %s", storePath, progress.Message)
		}
	}
}
