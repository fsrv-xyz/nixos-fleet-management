# nixos-fleet-management

Agents on NixOS hosts keep a bidirectional gRPC stream over mTLS to one of several
fleet servers. Servers share their state in etcd. Agents report facts (platform,
kernel, running NixOS generation) and switch to the system closure a CI assigns
to them; a web frontend shows agents, facts and deployments live.

## NixOS modules

```nix
inputs.fleet.url = "github:fsrv-xyz/nixos-fleet-management";
```

Control plane, next to an etcd cluster:

```nix
imports = [ inputs.fleet.nixosModules.server ];
services.fleet-server = {
  enable = true;
  openFirewall = true;
  etcdEndpoints = [ "10.0.0.1:2379" "10.0.0.2:2379" "10.0.0.3:2379" ];
  caCertificate = ./fleet-ca.crt;
  caKeyFile = "/run/secrets/fleet-ca-key";
  serverNames = [ "fleet1.example.org" ];
};
```

Every host:

```nix
imports = [ inputs.fleet.nixosModules.agent ];
services.fleet-agent = {
  enable = true;
  servers = [ "srv:_fleet._tcp.example.org" ];
  caServers = [ "srv:_fleet-ca._tcp.example.org" ];
  caCertificate = ./fleet-ca.crt;
};
```

The CA key is the only secret. It is read through systemd credentials, never
copied to the store, and only at start: restart `fleet-server.service` after
rotating it, e.g. with sops-nix `restartUnits`. Each server issues itself a
certificate for `serverNames` from the CA at start, with a key that stays in
memory, and replaces it before it expires.
The web frontend listens on `127.0.0.1:8080` without authentication.

## Operation

An agent without a certificate requests one and waits until it is signed:

```console
# fleet-ca list
node1 SHA256:4C0AE12A…
# fleet-ca sign node1
```

The agent renews its certificate after two thirds of its lifetime. Connected
over IPv4, it checks every five minutes whether its server answers over IPv6 and
reconnects once it does.

To deploy from CI, issue a deploy certificate on a server and store the output,
certificate and key, as a protected file variable of the pipeline:

```console
# fleet-ca deployer ci > ci.pem
# fleet-ca revoke ci
```

A deploy certificate may deploy to every agent and cannot connect as an agent;
reissuing or revoking it ends access at once. The pipeline builds a host's
system closure, pushes it to a cache the host substitutes from, and assigns it:

```console
$ nix build .#nixosConfigurations.node1.config.system.build.toplevel
$ nix copy --to <cache> ./result
$ echo '{"servers": ["srv:_fleet._tcp.example.org"], "ca": "fleet-ca.crt", "cert": "ci.pem", "key": "ci.pem"}' > deploy.json
$ fleet-deploy -config deploy.json -wait 15m node1 $(readlink ./result)
```

With `-wait`, `fleet-deploy` prints the agent's progress and fails if the
deployment fails or does not finish in time; the assignment stays either way.
On a server, the module's `fleet-deploy` wrapper assigns through etcd directly.

The agent fetches the closure, sets the system profile and runs
`switch-to-configuration switch` in the unit `nixos-rebuild-switch-to-configuration`,
whose journal holds the switch output.

## Development

```console
$ nix develop
$ etcd --data-dir .etcd
$ make generate-certificates
$ go run ./cmd/fleet-server
$ echo '{"certname": "dev"}' > agent.json
$ go run ./cmd/fleet-agent -config agent.json
$ go run ./cmd/fleet-server ca sign dev
```

All state lives in etcd below `etcdPrefix` (default `/fleet`).

All binaries take a single `-config` JSON file; its fields are the `config`
structs in `cmd/*`, unknown fields are rejected.
Without it, the defaults above apply.

`go generate ./api` regenerates the gRPC code; `go test ./...` runs the tests
against an embedded etcd.
