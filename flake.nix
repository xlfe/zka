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
            version = "0.4.0";
            src = ./.;
            vendorHash = null;
            subPackages = [ "cmd/zka" ];
            env.CGO_ENABLED = 0;
            ldflags = [ "-s" "-w" ];

            checkPhase = ''
              runHook preCheck
              go test ./...
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
            touch "$out"
          '';
        }
      );

      nixosModules.default = import ./nix/module.nix { inherit self; };
    };
}
