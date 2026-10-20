package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"time"

	"nixos-fleet-management/api"
	"nixos-fleet-management/fleet"
)

const connectTimeout = 30 * time.Second

type config struct {
	// Servers holds host:port addresses or a single "srv:<name>".
	Servers []string `json:"servers"`
	CA      string   `json:"ca"`
	// Cert and Key may name the same file holding both.
	Cert string `json:"cert"`
	Key  string `json:"key"`
}

func main() {
	path := flag.String("config", "", "JSON config file; without it, development defaults apply")
	wait := flag.Duration("wait", 0, "wait this long for the agent to finish the deployment")
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "usage: fleet-deploy [-config file] [-wait duration] <certname> <store path>")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 2 {
		flag.Usage()
		os.Exit(2)
	}

	cfg := config{
		Servers: []string{"localhost:8443"},
		CA:      "ca/ca.crt",
		Cert:    "deploy.pem",
		Key:     "deploy.pem",
	}
	if err := fleet.LoadConfig(*path, &cfg); err != nil {
		log.Fatal(err)
	}

	timeout := connectTimeout
	if *wait > 0 {
		timeout = *wait
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	servers, err := fleet.ResolveServers(ctx, net.DefaultResolver, cfg.Servers)
	if err != nil {
		log.Fatal(err)
	}
	err = fleet.RequestDeploy(ctx, servers, cfg.CA, cfg.Cert, cfg.Key, flag.Arg(0), flag.Arg(1), *wait > 0,
		func(p *api.DeployProgress) { fmt.Println(p.Phase, p.Message) })
	if err != nil {
		log.Fatal(err)
	}
}
