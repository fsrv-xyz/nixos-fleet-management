# CI-triggered deploys without etcd access

## Goal

A CI pipeline sets the desired system of an agent through the fleet servers'
public mTLS port, without access to etcd. It can optionally wait for the
outcome, bounded by a timeout.

## Constraints

- The pipeline reaches only the public ports, so it uses 8443 (agents).
- An agent certificate must never be able to deploy, and a deploy certificate
  must never be able to act as an agent.
- A deploy certificate may deploy to every agent.
- Getting the store path onto the agent (binary cache) is out of scope.

## Deploy certificates

`fleet-ca deployer <name>` runs on a server. It generates a key and a
certificate signed by the fleet CA:

- Subject `CN=<name>, OU=deploy`, EKU ClientAuth, valid 825 days like agent
  certificates.
- Stored under `/ca/deployers/<name>` in etcd.
- Key and certificate are printed once to stdout, for a protected CI variable.

`fleet-ca revoke <name>` deletes `/ca/deployers/<name>`. Access ends
immediately.

Agents cannot obtain `OU=deploy`: `issue()` sets only the CN of a CSR.

Expired deploy certificates are not renewed; a new one is issued.

## Authorization

- `Connect` and `RenewCertificate` reject certificates with `OU=deploy`
  (`PermissionDenied`).
- `Deploy` accepts only certificates with `OU=deploy` whose DER bytes equal
  the certificate stored under `/ca/deployers/<CN>`. Everything else gets
  `PermissionDenied`.

## API

```proto
service Deployments {
  rpc Deploy(DeployRequest) returns (stream DeployProgress);
}

message DeployRequest {
  string certname = 1;
  string store_path = 2;
  bool wait = 3;
}
```

`DeployProgress` is the existing message the agent reports and the server
stores under `/agents/<id>/deploy`.

The service is registered on the existing 8443 gRPC server, so
`srv:_fleet._tcp…` works unchanged.

Flow:

1. Validate `certname` and `store_path` with the patterns of `SetDesired`
   (`InvalidArgument`).
2. Reject a `certname` without an entry under `/ca/signed/` (`NotFound`), to
   catch typos in pipelines.
3. Write `/agents/<id>/desired` via `SetDesired` and log
   `deploy <id> <path> by <deployer>`.
4. Without `wait`, close the stream. With `wait`, watch `/agents/<id>/deploy`
   and forward progress for exactly this store path until DONE or FAILED.

The client's deadline bounds the wait; the server holds no timeout of its own.

## Client

New binary `cmd/fleet-deploy`:

```console
$ fleet-deploy -config deploy.json [-wait 15m] <certname> <store path>
```

- Config: `servers` (host:port or a single `srv:<name>`), `ca`, `cert`, `key`.
- Server resolution and failover reuse `ResolveServers` and `dial` from the
  agent.
- Prints each progress phase.
- Exit 0 on DONE (or after setting without `-wait`), 1 on FAILED, timeout or
  any error. A timeout leaves the desired path set; the agent still applies it.

The existing `fleet-server deploy` writing etcd directly stays for operators
on a server.

## Out of scope

- Restricting a deploy certificate to certain hosts.
- Several hosts per call; the pipeline calls the client per host.
- Renewal of deploy certificates.
- Binary cache.

## Testing

Integration tests with the embedded etcd, as the existing ones:

- A deploy certificate sets the desired path.
- An agent certificate gets `PermissionDenied` on `Deploy`.
- A revoked deploy certificate gets `PermissionDenied`.
- A deploy certificate is rejected by `Connect` and `RenewCertificate`.
- An unknown certname gets `NotFound`.
- `wait` ends with DONE once a fake agent reports progress for the path, and
  ignores progress for another path.
