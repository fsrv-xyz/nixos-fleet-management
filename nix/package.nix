{ lib, buildGo127Module }:

buildGo127Module {
  pname = "nixos-fleet-management";
  version = "0.1.0";

  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../go.mod
      ../go.sum
      ../api
      ../cmd
      ../fleet
    ];
  };

  vendorHash = "sha256-qlOoWfQvayZVd9NzcK53MmjLArPZyJSRqov1VIGkaaQ=";
  env.CGO_ENABLED = 0;
  ldflags = [
    "-s"
    "-w"
  ];

  meta = {
    description = "Fleet management for NixOS hosts over mTLS gRPC";
    platforms = lib.platforms.linux ++ lib.platforms.darwin;
  };
}
