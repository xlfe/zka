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
            version = "0.6.0";
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
                services.zka.ssh.identityAgent = "/run/user/%i/ssh-agent.socket";
                services.zka.ssh.forwardAgent = true;
                services.zka.ssh.extraOptions = [ "-o" "IdentitiesOnly=yes" ];
              }
            ];
          };
          disabledHooks = nixpkgs.lib.nixosSystem {
            modules = [
              self.nixosModules.default
              {
                nixpkgs.hostPlatform = system;
                system.stateVersion = "26.05";
                services.zka = {
                  enable = true;
                  codex.enableManagedHooks = false;
                  claude.enableManagedHooks = false;
                };
              }
            ];
          };
          service = evaluated.config.systemd.user.services.zkad;
          requirements = evaluated.config.environment.etc."codex/requirements.toml".source;
          claudeSettings = evaluated.config.environment.etc."claude-code/managed-settings.d/50-zka.json".source;
          disabledRuntimeConfig = disabledHooks.config.systemd.user.services.zkad.environment.ZKA_CONFIG;
          disabledCodexPresent = builtins.hasAttr "codex/requirements.toml" disabledHooks.config.environment.etc;
          disabledClaudePresent = builtins.hasAttr "claude-code/managed-settings.d/50-zka.json" disabledHooks.config.environment.etc;
        in
        {
          package = self.packages.${system}.zka;
          module = pkgs.runCommand "zka-module-check" {
            execStart = service.serviceConfig.ExecStart;
            runtimeConfig = service.environment.ZKA_CONFIG;
            inherit requirements claudeSettings;
            inherit disabledRuntimeConfig;
            disabledCodexPresent = toString disabledCodexPresent;
            disabledClaudePresent = toString disabledClaudePresent;
          } ''
            test -n "$execStart"
            grep -q '"fish"' "$runtimeConfig"
            grep -q 'ServerAliveInterval=5' "$runtimeConfig"
            grep -q 'IdentitiesOnly=yes' "$runtimeConfig"
            grep -q '/run/user/%i/ssh-agent.socket' "$runtimeConfig"
            grep -q '"forward_agent": *true' "$runtimeConfig"
            grep -q 'kitty-watcher.py' "$runtimeConfig"
            grep -q '"desktop_enabled": *true' "$runtimeConfig"
            grep -q '"ntfy_enabled": *true' "$runtimeConfig"
            grep -q '"ntfy_include_evidence": *false' "$runtimeConfig"
            grep -q '"blocked"' "$runtimeConfig"
            grep -q '"codex_managed_hooks": *true' "$runtimeConfig"
            grep -q '"claude_managed_hooks": *true' "$runtimeConfig"
            grep -q 'hook codex' "$requirements"
            grep -q 'managed_dir' "$requirements"
            grep -q 'hook claude' "$claudeSettings"
            grep -q 'AskUserQuestion|ExitPlanMode' "$claudeSettings"
            grep -q 'StopFailure' "$claudeSettings"
            grep -q '"codex_managed_hooks": *false' "$disabledRuntimeConfig"
            grep -q '"claude_managed_hooks": *false' "$disabledRuntimeConfig"
            test "$disabledCodexPresent" = ""
            test "$disabledClaudePresent" = ""
            test -x ${self.packages.${system}.zka}/bin/zka-launch
            touch "$out"
          '';
        }
      );

      nixosModules.default = import ./nix/module.nix { inherit self; };
    };
}
