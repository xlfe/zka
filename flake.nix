{
  description = "kitty-native orchestration for persistent coding-agent sessions";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
    in
    {
      packages = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        rec {
          zka = pkgs.buildGoModule {
            pname = "zka";
            version = "0.5.0";
            src = ./.;
            vendorHash = "sha256-IhE5JsdUYV1sRGOA2reDd7iLSJ7xF2IAqLpgD7JBXH0=";
            subPackages = [
              "cmd/zka"
              "cmd/zka-launch"
            ];
            tags = [ "nox11" ];
            env.CGO_ENABLED = 1;
            ldflags = [ "-s" "-w" ];

            nativeBuildInputs = [ pkgs.pkg-config ];
            buildInputs = [
              pkgs.libglvnd
              pkgs.libxkbcommon
              pkgs.vulkan-headers
              pkgs.wayland
            ];

            checkPhase = ''
              runHook preCheck
              go test -tags nox11 ./...
              runHook postCheck
            '';

            postInstall = ''
              mkdir -p "$out/libexec/zka/hooks"
              ln "$out/bin/zka" "$out/libexec/zka/hooks/zka"
              install -Dm0444 kitty/watcher.py "$out/share/zka/kitty-watcher.py"
            '';
          };
          default = zka;
        }
      );

      checks = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          evaluated = nixpkgs.lib.nixosSystem {
            modules = [
              self.nixosModules.default
              {
                nixpkgs.hostPlatform = system;
                system.stateVersion = "26.05";
                services.zka.enable = true;
              }
            ];
          };
          service = evaluated.config.systemd.user.services.zkad;
          requirements = evaluated.config.environment.etc."codex/requirements.toml".source;
        in
        {
          package = self.packages.${system}.zka;
          module = pkgs.runCommand "zka-module-check" {
            execStart = service.serviceConfig.ExecStart;
            runtimeConfig = service.environment.ZKA_CONFIG;
            inherit requirements;
          } ''
            test -n "$execStart"
            grep -q '"fish"' "$runtimeConfig"
            grep -q 'ServerAliveInterval=5' "$runtimeConfig"
            grep -q 'kitty-watcher.py' "$runtimeConfig"
            grep -q 'hook codex' "$requirements"
            grep -q 'managed_dir' "$requirements"
            test -x ${self.packages.${system}.zka}/bin/zka-launch
            touch "$out"
          '';
        }
      );

      nixosModules.default = import ./nix/module.nix { inherit self; };
    };
}
