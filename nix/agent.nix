{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.services.fleet-agent;
  stateDir = "/var/lib/fleet-agent";
  configFile = pkgs.writeText "fleet-agent.json" (
    builtins.toJSON {
      inherit (cfg) servers caServers certname;
      ca = "${cfg.caCertificate}";
      cert = "${stateDir}/agent.crt";
      key = "${stateDir}/agent.key";
    }
  );
in
{
  options.services.fleet-agent = {
    enable = lib.mkEnableOption "the fleet agent";

    package = lib.mkOption {
      type = lib.types.package;
      description = "Package providing `fleet-agent`.";
    };

    servers = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      example = [ "srv:_fleet._tcp.example.org" ];
      description = ''
        Agent endpoints of the fleet servers as `host:port`, or a single
        `srv:<name>` resolved through SRV records at start.
      '';
    };

    caServers = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      example = [ "srv:_fleet-ca._tcp.example.org" ];
      description = "Certificate request endpoints, in the same form as `servers`.";
    };

    caCertificate = lib.mkOption {
      type = lib.types.path;
      description = "CA certificate the servers' certificates are verified against.";
    };

    certname = lib.mkOption {
      type = lib.types.str;
      default = config.networking.hostName;
      defaultText = lib.literalExpression "config.networking.hostName";
      description = "Agent ID, the CN of the requested client certificate.";
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = builtins.match "[a-z0-9][a-z0-9.-]*" cfg.certname != null;
        message = "services.fleet-agent.certname must match [a-z0-9][a-z0-9.-]*, got \"${cfg.certname}\".";
      }
    ];

    systemd.services.fleet-agent = {
      description = "Fleet agent";
      wantedBy = [ "multi-user.target" ];
      wants = [ "network-online.target" ];
      after = [ "network-online.target" ];
      # Deployments run nix-store, nix-env and systemd-run as root, like nixos-rebuild.
      path = [
        config.nix.package
        config.systemd.package
      ];
      serviceConfig = {
        ExecStart = "${lib.getExe' cfg.package "fleet-agent"} -config ${configFile}";
        StateDirectory = "fleet-agent";
        StateDirectoryMode = "0700";
        Restart = "always";
        RestartSec = 5;
      };
    };
  };
}
