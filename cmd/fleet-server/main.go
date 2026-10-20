package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.etcd.io/etcd/client/v3/concurrency"
	"google.golang.org/grpc"

	"nixos-fleet-management/api"
	"nixos-fleet-management/fleet"
)

const (
	etcdTimeout    = 5 * time.Second
	etcdSessionTTL = 10
)

type config struct {
	// Name is shown in the web frontend for the agents connected to this server.
	Name       string   `json:"name"`
	Etcd       []string `json:"etcd"`
	EtcdPrefix string   `json:"etcdPrefix"`
	Listen     string   `json:"listen"`
	CAListen   string   `json:"caListen"`
	HTTPListen string   `json:"httpListen"`
	CACert     string   `json:"caCert"`
	CAKey      string   `json:"caKey"`
	// ServerNames are the DNS names and IP addresses agents reach this server by;
	// its certificate, issued at start, is valid for these.
	ServerNames []string `json:"serverNames"`
}

func main() {
	path := flag.String("config", "", "JSON config file; without it, development defaults apply")
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), `usage:
  fleet-server [-config file]                                 serve
  fleet-server [-config file] ca list | sign <certname>...    manage agent certificates
  fleet-server [-config file] ca deployer | revoke <name>     issue or revoke a deploy certificate
  fleet-server [-config file] deploy <certname> <store path>  assign a system closure`)
		flag.PrintDefaults()
	}
	flag.Parse()

	cfg := config{
		Etcd:        []string{"127.0.0.1:2379"},
		EtcdPrefix:  "/fleet",
		Listen:      ":8443",
		CAListen:    ":8444",
		HTTPListen:  ":8080",
		CACert:      "ca/ca.crt",
		CAKey:       "ca/ca.key",
		ServerNames: []string{"localhost", "127.0.0.1", "::1"},
	}
	if err := fleet.LoadConfig(*path, &cfg); err != nil {
		log.Fatal(err)
	}
	if cfg.Name == "" {
		cfg.Name = hostname()
	}

	args := flag.Args()
	switch {
	case len(args) == 0:
		serve(cfg)
	case args[0] == "ca":
		caCommand(cfg, args[1:])
	case args[0] == "deploy":
		deployCommand(cfg, args[1:])
	default:
		flag.Usage()
		os.Exit(2)
	}
}

func serve(cfg config) {
	etcd, err := fleet.NewEtcdClient(cfg.Etcd, cfg.EtcdPrefix)
	if err != nil {
		log.Fatal(err)
	}
	defer etcd.Close()
	session, err := concurrency.NewSession(etcd, concurrency.WithTTL(etcdSessionTTL))
	if err != nil {
		log.Fatal(err)
	}
	// Close revokes the lease, so this server's agents vanish at once instead of after the TTL.
	defer session.Close()

	ca, err := fleet.LoadCA(cfg.CACert, cfg.CAKey, etcd)
	if err != nil {
		log.Fatal(err)
	}
	serverCert, err := fleet.NewServerCertificate(ca, cfg.ServerNames)
	if err != nil {
		log.Fatal(err)
	}
	creds := fleet.ServerCredentials(ca, serverCert)
	caCreds := fleet.CAServerCredentials(serverCert)

	lis, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		log.Fatal(err)
	}
	caLis, err := net.Listen("tcp", cfg.CAListen)
	if err != nil {
		log.Fatal(err)
	}
	fleetServer := fleet.NewServer(ca, etcd, cfg.Name, session.Lease())
	srv := fleet.NewGRPCServer(creds, fleetServer)
	caSrv := grpc.NewServer(grpc.Creds(caCreds))
	api.RegisterCertificateAuthorityServer(caSrv, ca)

	httpSrv := &http.Server{Addr: cfg.HTTPListen, Handler: fleetServer.HTTPHandler()}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		select {
		case <-ctx.Done():
		case <-session.Done():
			log.Fatal("etcd session lost")
		}
		// GracefulStop would wait forever on the agents' endless streams.
		srv.Stop()
		caSrv.Stop()
		httpSrv.Close()
	}()
	go fleetServer.WatchAgents(ctx)
	go serverCert.Run(ctx)
	go func() {
		log.Printf("web frontend on %s", httpSrv.Addr)
		if err := httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	go func() {
		log.Printf("certificate requests on %s", caLis.Addr())
		if err := caSrv.Serve(caLis); err != nil {
			log.Fatal(err)
		}
	}()

	log.Printf("agents on %s", lis.Addr())
	if err := srv.Serve(lis); err != nil {
		log.Fatal(err)
	}
}

func caCommand(cfg config, args []string) {
	list := len(args) == 1 && args[0] == "list"
	sign := len(args) > 1 && args[0] == "sign"
	deployer := len(args) == 2 && args[0] == "deployer"
	revoke := len(args) == 2 && args[0] == "revoke"
	if !list && !sign && !deployer && !revoke {
		flag.Usage()
		os.Exit(2)
	}
	etcd, err := fleet.NewEtcdClient(cfg.Etcd, cfg.EtcdPrefix)
	if err != nil {
		log.Fatal(err)
	}
	defer etcd.Close()
	ca, err := fleet.LoadCA(cfg.CACert, cfg.CAKey, etcd)
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), etcdTimeout)
	defer cancel()
	switch {
	case list:
		if err := ca.List(ctx, os.Stdout); err != nil {
			log.Fatal(err)
		}
	case deployer:
		certPEM, keyPEM, err := ca.IssueDeployer(ctx, args[1])
		if err != nil {
			log.Fatal(err)
		}
		os.Stdout.Write(certPEM)
		os.Stdout.Write(keyPEM)
	case revoke:
		if err := ca.RevokeDeployer(ctx, args[1]); err != nil {
			log.Fatal(err)
		}
	default:
		for _, name := range args[1:] {
			if err := ca.Sign(ctx, name); err != nil {
				log.Fatal(err)
			}
			fmt.Printf("signed %s\n", name)
		}
	}
}

func deployCommand(cfg config, args []string) {
	if len(args) != 2 {
		flag.Usage()
		os.Exit(2)
	}
	etcd, err := fleet.NewEtcdClient(cfg.Etcd, cfg.EtcdPrefix)
	if err != nil {
		log.Fatal(err)
	}
	defer etcd.Close()
	ctx, cancel := context.WithTimeout(context.Background(), etcdTimeout)
	defer cancel()
	if err := fleet.SetDesired(ctx, etcd, args[0], args[1]); err != nil {
		log.Fatal(err)
	}
}

func hostname() string {
	name, err := os.Hostname()
	if err != nil {
		log.Fatal(err)
	}
	return name
}
