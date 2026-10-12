{
  description = "Fleet management for NixOS hosts over mTLS gRPC";
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" "aarch64-darwin" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems(system: f nixpkgs.legacyPackages.${system});
      withPackage = service: module: { lib, pkgs, ... }: {
        imports = [ module ];
        services.${service}.package = lib.mkDefault self.packages.${pkgs.stdenv.hostPlatform.system}.default;
      };
    in
    {
      packages = forAllSystems (pkgs: {
        default = pkgs.callPackage ./nix/package.nix { };
      });

      # nix flake check only evaluates packages; listed here, it builds and tests them.
      checks = forAllSystems (pkgs: {
        package = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
      });

      nixosModules = {
        agent = withPackage "fleet-agent" ./nix/agent.nix;
        server = withPackage "fleet-server" ./nix/server.nix;
      };

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell{
          packages = with pkgs; [
            gnumake
            go_1_27
            protobuf
            protoc-gen-go
            protoc-gen-go-grpc
            openssl
            etcd
          ];
        };
      });
    };
}
