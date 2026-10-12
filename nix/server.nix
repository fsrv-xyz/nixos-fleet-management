{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.services.fleet-server;
  credentials = "/run/credentials/fleet-server.service";
  settings = {
    inherit (cfg) name;
    etcd = cfg.etcdEndpoints;
    inherit (cfg) etcdPrefix;
    listen = ":${toString cfg.port}";
    caListen = ":${toString cfg.caPort}";
    httpListen = cfg.httpListen;
    caCert = "${cfg.caCertificate}";
    cert = "${cfg.certificate}";
  };
  configFile = pkgs.writeText "fleet-server.json" (
    builtins.toJSON (
      settings
      // {
        caKey = "${credentials}/ca.key";
        key = "${credentials}/server.key";
      }
    )
  );
  # fleet-ca runs outside the unit and reads the key files directly, as root.
  cliConfig = pkgs.writeText "fleet-server-cli.json" (
    builtins.toJSON (
      settings
      // {
        caKey = cfg.caKeyFile;
        key = cfg.keyFile;
      }
    )
  );
  fleetServer = lib.getExe' cfg.package "fleet-server";
in
{
  options.services.fleet-server = {
    enable = lib.mkEnableOption "the fleet server";

    package = lib.mkOption {
      type = lib.types.package;
      description = "Package providing `fleet-server`.";
    };

    name = lib.mkOption {
      type = lib.types.str;
      default = config.networking.hostName;
      defaultText = lib.literalExpression "config.networking.hostName";
      description = "Server name shown in the web frontend for the agents connected to it.";
    };

    etcdEndpoints = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ "127.0.0.1:2379" ];
      description = "etcd endpoints; list every member, so the server fails over.";
    };

    etcdPrefix = lib.mkOption {
      type = lib.types.str;
      default = "/fleet";
      description = "Prefix of all keys, so several fleets can share one etcd cluster.";
    };

    port = lib.mkOption {
      type = lib.types.port;
      default = 8443;
      description = "Agent endpoint, requires client certificates.";
    };

    caPort = lib.mkOption {
      type = lib.types.port;
      default = 8444;
      description = "Certificate request endpoint for agents without a certificate.";
    };

    httpListen = lib.mkOption {
      type = lib.types.str;
      default = "127.0.0.1:8080";
      description = "Address of the web frontend, which has no authentication.";
    };

    openFirewall = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = "Open `port` and `caPort`; the web frontend stays closed.";
    };

    caCertificate = lib.mkOption {
      type = lib.types.path;
      description = "CA certificate.";
    };

    caKeyFile = lib.mkOption {
      type = lib.types.externalPath;
      example = "/run/secrets/fleet-ca-key";
      description = "CA key, read through systemd credentials, never copied to the store.";
    };

    certificate = lib.mkOption {
      type = lib.types.path;
      description = "Server certificate, with every name agents use to reach this server.";
    };

    keyFile = lib.mkOption {
      type = lib.types.externalPath;
      example = "/run/secrets/fleet-server-key";
      description = "Server key, read through systemd credentials, never copied to the store.";
    };
  };

  config = lib.mkIf cfg.enable {
    networking.firewall.allowedTCPPorts = lib.mkIf cfg.openFirewall [
      cfg.port
      cfg.caPort
    ];

    environment.systemPackages = [
      (pkgs.writeShellScriptBin "fleet-ca" ''exec ${fleetServer} -config ${cliConfig} ca "$@"'')
      (pkgs.writeShellScriptBin "fleet-deploy" ''exec ${fleetServer} -config ${cliConfig} deploy "$@"'')
    ];

    systemd.services.fleet-server = {
      description = "Fleet server";
      wantedBy = [ "multi-user.target" ];
      wants = [ "network-online.target" ];
      after = [ "network-online.target" ];
      serviceConfig = {
        ExecStart = "${fleetServer} -config ${configFile}";
        LoadCredential = [
          "ca.key:${cfg.caKeyFile}"
          "server.key:${cfg.keyFile}"
        ];
        DynamicUser = true;
        # Exits when it loses its etcd session; agents reconnect to the restarted server.
        Restart = "always";
        RestartSec = 5;
      };
    };
  };
}
