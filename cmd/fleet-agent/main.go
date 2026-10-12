package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"nixos-fleet-management/fleet"
)

type config struct {
	// Servers and CAServers hold host:port addresses or a single "srv:<name>".
	Servers     []string       `json:"servers"`
	CAServers   []string       `json:"caServers"`
	Certname    string         `json:"certname"`
	CA          string         `json:"ca"`
	Cert        string         `json:"cert"`
	Key         string         `json:"key"`
	WaitForCert fleet.Duration `json:"waitForCert"`
	Interval    fleet.Duration `json:"interval"`
}

func main() {
	path := flag.String("config", "", "JSON config file; without it, development defaults apply")
	flag.Parse()

	cfg := config{
		Servers:     []string{"localhost:8443"},
		CAServers:   []string{"localhost:8444"},
		CA:          "ca/ca.crt",
		Cert:        "agent/agent.crt",
		Key:         "agent/agent.key",
		WaitForCert: fleet.Duration(30 * time.Second),
		Interval:    fleet.Duration(10 * time.Second),
	}
	if err := fleet.LoadConfig(*path, &cfg); err != nil {
		log.Fatal(err)
	}
	if cfg.Certname == "" {
		hostname, err := os.Hostname()
		if err != nil {
			log.Fatal(err)
		}
		cfg.Certname = hostname
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, cfg); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}

func run(ctx context.Context, cfg config) error {
	caServers, err := resolve(ctx, cfg.CAServers)
	if err != nil {
		return err
	}
	if err := fleet.EnsureCertificate(ctx, caServers, cfg.CA, cfg.Cert, cfg.Key, cfg.Certname, time.Duration(cfg.WaitForCert)); err != nil {
		return err
	}
	servers, err := resolve(ctx, cfg.Servers)
	if err != nil {
		return err
	}
	return fleet.RunAgent(ctx, servers, cfg.CA, cfg.Cert, cfg.Key, time.Duration(cfg.Interval))
}

func resolve(ctx context.Context, servers []string) ([]string, error) {
	addrs, err := fleet.ResolveServers(ctx, net.DefaultResolver, servers)
	if err != nil {
		return nil, err
	}
	log.Printf("servers for %s: %s", strings.Join(servers, ", "), strings.Join(addrs, ", "))
	return addrs, nil
}
