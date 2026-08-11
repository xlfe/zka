{
  description = "Kitty-native durable workspaces, agent attention, and scoped remote credentials";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
      releaseVersion = "0.9.1";
    in
    {
      # kitty and python3 are what the differential session-parser oracle in
      # internal/zka needs; without them those tests skip rather than fail.
      devShells = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        {
          default = pkgs.mkShell {
            packages = [
              pkgs.go
              pkgs.gopls
              pkgs.kitty
              pkgs.python3
              pkgs.gnupg
              pkgs.git
              pkgs.pkg-config
              pkgs.shellcheck
              pkgs.libglvnd
              pkgs.libxkbcommon
              pkgs.vulkan-headers
              pkgs.wayland
            ];
            ZKA_KITTY_LIB = "${pkgs.kitty}/lib/kitty";
            shellHook = ''
              # Linux AF_UNIX paths are limited to 108 bytes. Nix's default
              # /tmp/nix-shell.* TMPDIR makes Go's test-name directories too long.
              export TMPDIR=/tmp
            '';
          };
        }
      );

      packages = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        rec {
          zka = pkgs.buildGoModule {
            pname = "zka";
            version = releaseVersion;
            src = ./.;
            vendorHash = "sha256-rf+/xKWBQnZ+CWuECCoeTR/64aQBRvzUHK5XN22zhoc=";
            subPackages = [
              "cmd/zka"
              "cmd/zka-launch"
            ];
            tags = [ "nox11" ];
            env.CGO_ENABLED = 1;
            ldflags = [ "-s" "-w" ];

            nativeBuildInputs = [ pkgs.pkg-config pkgs.gnupg pkgs.git ];
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

          # The view layer never runs on a headless origin, so this variant
          # drops cmd/zka-launch and with it the whole Gio/Wayland/Vulkan
          # closure. The daemon and CLI are pure Go, so CGO is off entirely.
          zka-headless = pkgs.buildGoModule {
            pname = "zka-headless";
            version = releaseVersion;
            src = ./.;
            vendorHash = "sha256-rf+/xKWBQnZ+CWuECCoeTR/64aQBRvzUHK5XN22zhoc=";
            subPackages = [ "cmd/zka" ];
            env.CGO_ENABLED = 0;
            ldflags = [ "-s" "-w" ];
            nativeBuildInputs = [ pkgs.gnupg pkgs.git ];

            # The full package's check already runs the whole suite; this one
            # proves the CGO-free build passes its own packages (the launcher
            # package would need the Gio C libraries this variant exists to
            # avoid).
            checkPhase = ''
              runHook preCheck
              go test ./cmd/zka ./internal/zka
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
          credentialBundlesModule = {
            services.zka.credentials = {
              defaultBundle = "work";
              bundles.work = {
                sshAgent.enable = true;
                openpgp = {
                  enable = true;
                };
              };
            };
          };
          evaluated = nixpkgs.lib.nixosSystem {
            modules = [
              self.nixosModules.default
              credentialBundlesModule
              {
                nixpkgs.hostPlatform = system;
                system.stateVersion = "26.05";
                services.zka.enable = true;
                services.zka.ssh.identityAgent = "/run/user/%i/ssh-agent.socket";
                services.zka.ssh.extraOptions = [ "-o" "IdentitiesOnly=yes" ];
                services.zka.ssh.expectedNodeIDs.devbox = "0123456789abcdef0123456789abcdef";
                services.zka.credentials.bundles.work.openpgp.signingKeys = [ "1111222233334444555566667777888899990000" ];
                services.zka.credentials.providers.laptop = {
                  nodeID = "fedcba9876543210fedcba9876543210";
                  sshSourceAddresses = [ "192.0.2.10" ];
                };
                services.zka.credentials.gnupg.configureAgent = true;
              # A stand-in for pkgs.sway: what matters is that an absolute store
              # path reaches the runtime config, because zkad's PATH comes from
              # the unit and a bare "swaymsg" does not resolve there.
              services.zka.sway.package = pkgs.writeShellScriptBin "swaymsg" "";
              }
            ];
          };
          disabledHooks = nixpkgs.lib.nixosSystem {
            modules = [
              self.nixosModules.default
              credentialBundlesModule
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
          headless = nixpkgs.lib.nixosSystem {
            modules = [
              self.nixosModules.default
              credentialBundlesModule
              {
                nixpkgs.hostPlatform = system;
                system.stateVersion = "26.05";
                users.users.agents = {
                  isNormalUser = true;
                  group = "users";
                };
                services.zka = {
                  enable = true;
                  headless = true;
                  linger.users = [ "agents" ];
                  # A stand-in for the operator's ntfy helper: what matters is
                  # that an absolute store path reaches the runtime config and
                  # the package lands on zkad's PATH.
                  notifications.ntfyPackage = pkgs.writeShellScriptBin "ntfy-send" "";
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
          headlessService = headless.config.systemd.user.services.zkad;
        in
        {
          package = self.packages.${system}.zka;
          module = pkgs.runCommand "zka-module-check" {
            execStart = service.serviceConfig.ExecStart;
            runtimeConfig = service.environment.ZKA_CONFIG;
            readme = ./README.md;
            inherit releaseVersion;
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
            grep -q '0123456789abcdef0123456789abcdef' "$runtimeConfig"
            grep -q 'fedcba9876543210fedcba9876543210' "$runtimeConfig"
            grep -q '192.0.2.10' "$runtimeConfig"
            grep -q '"default_bundle": *"work"' "$runtimeConfig"
            grep -q '"ssh_agent"' "$runtimeConfig"
            grep -q '"openpgp"' "$runtimeConfig"
            grep -q '1111222233334444555566667777888899990000' "$runtimeConfig"
            grep -qE '"gpgconf_command": *"/nix/store/[^\"]*/bin/gpgconf"' "$runtimeConfig"
            grep -q 'kitty-watcher.py' "$runtimeConfig"
            grep -q '"desktop_enabled": *true' "$runtimeConfig"
            grep -q '"ntfy_enabled": *true' "$runtimeConfig"
            grep -q '"ntfy_include_evidence": *false' "$runtimeConfig"
            grep -qE '"sway_command": *"/nix/store/[^"]*/bin/swaymsg"' "$runtimeConfig"
            grep -q '"sway_command": *"swaymsg"' "$disabledRuntimeConfig"
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
            grep -q '"headless": *false' "$runtimeConfig"
            test "$disabledCodexPresent" = ""
            test "$disabledClaudePresent" = ""
            test -x ${self.packages.${system}.zka}/bin/zka-launch
            test "$(${self.packages.${system}.zka}/bin/zka --version)" = "$releaseVersion"
            test "$(${self.packages.${system}.zka-headless}/bin/zka --version)" = "$releaseVersion"
            grep -q "zka $releaseVersion is pre-1.0" "$readme"
            touch "$out"
          '';
          headless-module = pkgs.runCommand "zka-headless-module-check" {
            headlessRuntimeConfig = headlessService.environment.ZKA_CONFIG;
            headlessPath = toString headlessService.path;
            headlessExecStart = headlessService.serviceConfig.ExecStart;
            headlessLinger = toString headless.config.users.users.agents.linger;
          } ''
            grep -q '"headless": *true' "$headlessRuntimeConfig"
            grep -q '"command": *"kitty"' "$headlessRuntimeConfig"
            grep -q '"kitten_command": *"kitten"' "$headlessRuntimeConfig"
            grep -q '"desktop_enabled": *false' "$headlessRuntimeConfig"
            grep -qE '"ntfy_command": *"/nix/store/[^"]*/bin/ntfy-send"' "$headlessRuntimeConfig"
            if echo "$headlessPath" | grep -q -- '-kitty-'; then
              echo "headless servicePath still carries the Kitty closure" >&2
              exit 1
            fi
            echo "$headlessExecStart" | grep -q 'zka-headless'
            test "$headlessLinger" = "1"
            touch "$out"
          '';
          headless-package = pkgs.runCommand "zka-headless-package-check" { } ''
            test -x ${self.packages.${system}.zka-headless}/bin/zka
            test ! -e ${self.packages.${system}.zka-headless}/bin/zka-launch
            test -x ${self.packages.${system}.zka-headless}/libexec/zka/hooks/zka
            test -f ${self.packages.${system}.zka-headless}/share/zka/kitty-watcher.py
            touch "$out"
          '';
        }
      );

      nixosModules.default = import ./nix/module.nix { inherit self; };
    };
}
