{ self }:
{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.services.zka;
  toml = pkgs.formats.toml { };
  json = pkgs.formats.json { };
  codexHookCommand = "${cfg.package}/libexec/zka/hooks/zka hook codex";
  codexHook = {
    hooks = [
      {
        type = "command";
        command = codexHookCommand;
        timeout = 2;
      }
    ];
  };
  claudeHookCommand = "${cfg.package}/libexec/zka/hooks/zka hook claude";
  claudeHook = {
    hooks = [
      {
        type = "command";
        command = claudeHookCommand;
        timeout = 2;
      }
    ];
  };
  zkaRequirements = {
    features.hooks = true;
    hooks = {
      managed_dir = "${cfg.package}/libexec/zka/hooks";
      SessionStart = [
        (codexHook // { matcher = "startup|resume|clear|compact"; })
      ];
      UserPromptSubmit = [ codexHook ];
      PermissionRequest = [
        (codexHook // { matcher = ".*"; })
      ];
      PostToolUse = [
        (codexHook // { matcher = ".*"; })
      ];
      Stop = [ codexHook ];
    };
  };
  requirements = lib.recursiveUpdate cfg.codex.extraRequirements zkaRequirements;
  requirementsFile = toml.generate "zka-codex-requirements.toml" requirements;
  claudeManagedSettings = {
    hooks = {
      SessionStart = [
        (claudeHook // { matcher = "startup|resume|clear|compact"; })
      ];
      UserPromptSubmit = [ claudeHook ];
      PreToolUse = [
        (claudeHook // { matcher = "AskUserQuestion|ExitPlanMode"; })
      ];
      PermissionRequest = [
        (claudeHook // { matcher = "*"; })
      ];
      PostToolUse = [
        (claudeHook // { matcher = "*"; })
      ];
      PostToolUseFailure = [
        (claudeHook // { matcher = "*"; })
      ];
      Elicitation = [
        (claudeHook // { matcher = "*"; })
      ];
      ElicitationResult = [
        (claudeHook // { matcher = "*"; })
      ];
      Notification = [
        (claudeHook // {
          matcher = "permission_prompt|idle_prompt|elicitation_dialog|elicitation_complete|elicitation_response";
        })
      ];
      Stop = [ claudeHook ];
      StopFailure = [
        (claudeHook // { matcher = "*"; })
      ];
      SessionEnd = [
        (claudeHook // { matcher = "*"; })
      ];
    };
  };
  claudeManagedSettingsFile = json.generate "zka-claude-managed-settings.json" claudeManagedSettings;
  reservedRequirementPaths = [
    [ "features" "hooks" ]
    [ "hooks" "managed_dir" ]
    [ "hooks" "SessionStart" ]
    [ "hooks" "UserPromptSubmit" ]
    [ "hooks" "PermissionRequest" ]
    [ "hooks" "PostToolUse" ]
    [ "hooks" "Stop" ]
  ];
  zmxCommand =
    if cfg.zmx.package == null then
      "zmx"
    else
      "${cfg.zmx.package}/bin/zmx";
  ntfyCommand =
    if cfg.notifications.ntfyPackage == null then
      cfg.notifications.ntfyCommand
    else
      "${cfg.notifications.ntfyPackage}/bin/${cfg.notifications.ntfyCommand}";
  pivbCommand =
    if cfg.credentials.pivb.package == null then
      "pivb"
    else
      "${cfg.credentials.pivb.package}/bin/pivb";
  anyOpenPGPBundle = lib.any (bundle: bundle.openpgp.enable) (lib.attrValues cfg.credentials.bundles);
  forbiddenForwardAgent = lib.any (
    option:
    let
      lowered = lib.toLower option;
    in
    option == "-A"
    || builtins.match "^-o *forwardagent[ =]+yes$" lowered != null
    || builtins.match "^forwardagent[ =]+yes$" lowered != null
  ) (cfg.ssh.options ++ cfg.ssh.extraOptions);
  credentialBundleAssertions = lib.concatLists (lib.mapAttrsToList (name: bundle: [
    {
      assertion = builtins.match "^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$" name != null;
      message = "services.zka.credentials bundle names must start with an alphanumeric character and contain at most 64 letters, digits, dots, underscores, or hyphens";
    }
    {
      assertion = bundle.sshAgent.enable || bundle.openpgp.enable || bundle.pivb.enable;
      message = "services.zka.credentials.bundles.${name} must enable at least one capability";
    }
    {
      assertion = lib.all (fingerprint: builtins.match "^[0-9A-Fa-f]{40}([0-9A-Fa-f]{24})?$" fingerprint != null) bundle.openpgp.signingKeys;
      message = "services.zka.credentials.bundles.${name}.openpgp.signingKeys must contain full 40- or 64-hex-character fingerprints";
    }
    {
      assertion = !bundle.pivb.enable || bundle.pivb.aliases != [ ];
      message = "services.zka.credentials.bundles.${name}.pivb.aliases must be non-empty when PIVB is enabled";
    }
    {
      assertion = lib.all (alias: builtins.match "^[a-z]([a-z0-9-]{0,30}[a-z0-9])?$" alias != null) bundle.pivb.aliases;
      message = "services.zka.credentials.bundles.${name}.pivb.aliases contains an invalid PIVB alias";
    }
    {
      assertion = lib.unique bundle.pivb.aliases == bundle.pivb.aliases;
      message = "services.zka.credentials.bundles.${name}.pivb.aliases must not contain duplicates";
    }
  ]) cfg.credentials.bundles);
  credentialProviderAssertions = lib.concatLists (lib.mapAttrsToList (name: provider: [
    {
      assertion = builtins.match "^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$" name != null;
      message = "services.zka.credentials provider names must start with an alphanumeric character and contain at most 64 letters, digits, dots, underscores, or hyphens";
    }
    {
      assertion = builtins.match "^[0-9a-f]{32}$" provider.nodeID != null;
      message = "services.zka.credentials.providers.${name}.nodeID must be a 32-character lowercase hexadecimal zka node ID";
    }
  ]) cfg.credentials.providers);
  runtimeConfig = json.generate "zka-config.json" {
    headless = cfg.headless;
    attention.states = cfg.attention.states;
    shell.command = cfg.shell.command;
    kitty = {
      # A headless origin never executes the view layer, and LoadConfig
      # rejects empty command strings, so the bare names stand in and keep
      # the Kitty closure off the machine entirely.
      command = if cfg.headless then "kitty" else "${cfg.kitty.package}/bin/kitty";
      kitten_command = if cfg.headless then "kitten" else "${cfg.kitty.package}/bin/kitten";
      watcher = toString cfg.kitty.watcher;
      extra_args = cfg.kitty.extraArgs;
    };
    zmx.command = zmxCommand;
    ssh = {
      command = "${cfg.ssh.package}/bin/ssh";
      options = cfg.ssh.options ++ cfg.ssh.extraOptions;
      identity_agent = cfg.ssh.identityAgent;
      expected_node_ids = cfg.ssh.expectedNodeIDs;
    };
    credentials = {
      default_bundle = if cfg.credentials.defaultBundle == null then "" else cfg.credentials.defaultBundle;
      gnupg = {
        command = "${cfg.credentials.gnupg.package}/bin/gpg";
        gpgconf_command = "${cfg.credentials.gnupg.package}/bin/gpgconf";
        gpg_connect_agent_command = "${cfg.credentials.gnupg.package}/bin/gpg-connect-agent";
        configure_agent = cfg.credentials.gnupg.configureAgent;
        operation_timeout = cfg.credentials.gnupg.operationTimeout;
      };
      pivb = {
        forward_socket = if cfg.credentials.pivb.forwardSocket == null then "" else cfg.credentials.pivb.forwardSocket;
        command = pivbCommand;
        routing_mode = cfg.credentials.pivb.routingMode;
      };
      bundles = lib.mapAttrs (_: bundle: {
        ssh_agent.enable = bundle.sshAgent.enable;
        openpgp = {
          enable = bundle.openpgp.enable;
          signing_keys = bundle.openpgp.signingKeys;
        };
        pivb = {
          enable = bundle.pivb.enable;
          aliases = bundle.pivb.aliases;
        };
      }) cfg.credentials.bundles;
      providers = lib.mapAttrs (_: provider: {
        node_id = provider.nodeID;
        ssh_source_addresses = provider.sshSourceAddresses;
      }) cfg.credentials.providers;
    };
    notifications = {
      desktop_enabled = cfg.notifications.desktopEnabled;
      ntfy_enabled = cfg.notifications.ntfyEnabled;
      ntfy_include_evidence = cfg.notifications.ntfyIncludeEvidence;
      ntfy_command = ntfyCommand;
    };
    focus.sway_command =
      if cfg.sway.package == null then "swaymsg" else "${cfg.sway.package}/bin/swaymsg";
    integrations = {
      codex_managed_hooks = cfg.codex.enableManagedHooks;
      claude_managed_hooks = cfg.claude.enableManagedHooks;
    };
  };
  servicePath = [
    cfg.package
    cfg.shell.package
    cfg.ssh.package
  ]
  ++ lib.optional (!cfg.headless) cfg.kitty.package
  ++ lib.optional (cfg.zmx.package != null) cfg.zmx.package
  ++ lib.optional (cfg.sway.package != null) cfg.sway.package
  ++ lib.optional (cfg.notifications.ntfyPackage != null) cfg.notifications.ntfyPackage
  ++ lib.optional (cfg.credentials.pivb.package != null) cfg.credentials.pivb.package
  ++ lib.optional (anyOpenPGPBundle || cfg.credentials.gnupg.configureAgent) cfg.credentials.gnupg.package
  ++ cfg.extraPackages;
in
{
  options.services.zka = {
    enable = lib.mkEnableOption "zka Kitty workspace orchestration";

    headless = lib.mkEnableOption ''
      headless origin mode: this machine never hosts a Kitty view and runs
      only zkad, zmx, sshd, and the agents inside its panes. Kitty leaves the
      service path and closure, desktop notifications default off, doctor
      skips the view-layer checks, and workspaces created here are attached
      from other machines over SSH
    '';

    package = lib.mkOption {
      type = lib.types.package;
      default =
        if cfg.headless then
          self.packages.${pkgs.stdenv.hostPlatform.system}.zka-headless
        else
          self.packages.${pkgs.stdenv.hostPlatform.system}.default;
      defaultText = lib.literalExpression ''self.packages.''${pkgs.stdenv.hostPlatform.system}.''${if config.services.zka.headless then "zka-headless" else "default"}'';
      description = "The zka package to run. Headless origins default to the launcher-free zka-headless variant.";
    };

    shell = {
      package = lib.mkPackageOption pkgs "fish" { };

      command = lib.mkOption {
        type = lib.types.nonEmptyListOf lib.types.str;
        default = [ "fish" ];
        description = "Command started inside each new zmx-backed workspace pane.";
      };
    };

    kitty = {
      package = lib.mkPackageOption pkgs "kitty" { };

      extraArgs = lib.mkOption {
        type = lib.types.listOf lib.types.str;
        default = [ ];
        description = "Additional safe options passed to every dedicated managed Kitty process.";
      };

      watcher = lib.mkOption {
        type = lib.types.path;
        default = "${cfg.package}/share/zka/kitty-watcher.py";
        defaultText = lib.literalExpression ''"\${config.services.zka.package}/share/zka/kitty-watcher.py"'';
        description = "Global Kitty watcher used to trigger authoritative topology captures.";
      };
    };

    zmx.package = lib.mkOption {
      type = lib.types.nullOr lib.types.package;
      default = null;
      description = "Optional zmx package; leave null when zmx is supplied system-wide.";
    };

    sway = {
      package = lib.mkOption {
        type = lib.types.nullOr lib.types.package;
        default = if config.programs.sway.enable then pkgs.sway else null;
        defaultText = lib.literalExpression "if config.programs.sway.enable then pkgs.sway else null";
        description = ''
          Package providing swaymsg, used to raise the Kitty window owning a pane
          when a desktop notification is actioned. zkad runs from a systemd unit
          whose PATH is this module's, not a login shell's, so a bare `swaymsg`
          would not resolve; the absolute store path is written into the runtime
          configuration. Defaults to null on hosts that do not enable Sway, where
          compositor focus is a no-op anyway.
        '';
      };
    };

    ssh = {
      package = lib.mkPackageOption pkgs "openssh" { };

      identityAgent = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        example = "/run/user/%i/ssh-agent.socket";
        description = "Persistent OpenSSH IdentityAgent used by zkad and remote pane attachments; supports OpenSSH tokens such as %i.";
      };

      options = lib.mkOption {
        type = lib.types.listOf lib.types.str;
        default = [
          "-o"
          "ServerAliveInterval=5"
          "-o"
          "ServerAliveCountMax=3"
          "-o"
          "BatchMode=yes"
        ];
        description = "OpenSSH options used for remote workspace control and pane attachment.";
      };

      extraOptions = lib.mkOption {
        type = lib.types.listOf lib.types.str;
        default = [ ];
        description = "Additional OpenSSH options appended to the default ssh.options list.";
      };

      expectedNodeIDs = lib.mkOption {
        type = lib.types.attrsOf lib.types.str;
        default = { };
        example.devbox = "0123456789abcdef0123456789abcdef";
        description = "Expected authoritative zka node ID for each outbound SSH host alias; required when this host provides credential bundles.";
      };
    };

    credentials = {
      defaultBundle = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        example = "work";
        description = "Credential bundle selected when a claim omits --bundle; this does not claim credentials automatically.";
      };

      gnupg = {
        package = lib.mkPackageOption pkgs "gnupg" { };

        configureAgent = lib.mkOption {
          type = lib.types.bool;
          default = false;
          description = ''
            Configure this provider host's global user gpg-agent, restricted
            extra socket, and pinentry. This takes ownership of a globally
            shared agent and may interact with other smart-card consumers.
          '';
        };

        pinentryPackage = lib.mkOption {
          type = lib.types.package;
          default = pkgs.pinentry-gnome3;
          defaultText = lib.literalExpression "pkgs.pinentry-gnome3";
          description = "Graphical pinentry package configured for the provider's global gpg-agent.";
        };

        operationTimeout = lib.mkOption {
          type = lib.types.str;
          default = "45s";
          description = "Go duration bounding each forwarded private-key operation.";
        };
      };

      pivb = {
        package = lib.mkOption {
          type = lib.types.nullOr lib.types.package;
          default = null;
          description = "PIVB package used for managed-pane capability negotiation; null resolves pivb from PATH.";
        };

        forwardSocket = lib.mkOption {
          type = lib.types.nullOr lib.types.str;
          default = null;
          description = "Absolute provider-side pivbd forwarding socket; null uses $XDG_RUNTIME_DIR/pivb/forward.sock at runtime.";
        };

        routingMode = lib.mkOption {
          type = lib.types.enum [ "environment" ];
          default = "environment";
          description = "Managed PIVB route binding. Protocol 1 is cooperative; enforced provenance remains future work.";
        };
      };

      bundles = lib.mkOption {
        default = { };
        description = "Named, semantic credential capability bundles available for workspace claims.";
        type = lib.types.attrsOf (lib.types.submodule ({ ... }: {
          options = {
            sshAgent.enable = lib.mkOption {
              type = lib.types.bool;
              default = false;
              description = "Expose the provider's selected SSH agent through this bundle.";
            };
            openpgp.enable = lib.mkOption {
              type = lib.types.bool;
              default = false;
              description = "Expose filtered OpenPGP signing and decryption through this bundle.";
            };
            openpgp.signingKeys = lib.mkOption {
              type = lib.types.listOf lib.types.str;
              default = [ ];
              description = "Full OpenPGP fingerprints authorized when this node provides the bundle; leave empty on a target-only node.";
            };
            pivb.enable = lib.mkOption {
              type = lib.types.bool;
              default = false;
              description = "Expose the provider's constrained PIVB mint capability through this workspace bundle.";
            };
            pivb.aliases = lib.mkOption {
              type = lib.types.listOf lib.types.str;
              default = [ ];
              description = "PIVB aliases authorized through this bundle; the active card identity is pinned when claimed.";
            };
          };
        }));
      };

      providers = lib.mkOption {
        default = { };
        description = "Credential-provider node IDs trusted by this target, with optional SSH source-address restrictions for fixed hosts.";
        type = lib.types.attrsOf (lib.types.submodule ({ ... }: {
          options = {
            nodeID = lib.mkOption {
              type = lib.types.str;
              description = "Pinned 32-character zka node ID advertised by this provider.";
            };
            sshSourceAddresses = lib.mkOption {
              type = lib.types.listOf lib.types.str;
              default = [ ];
              example = [ "192.0.2.10" "2001:db8::/64" ];
              description = "Optional IP addresses or CIDR networks from which this provider may establish SSH_CONNECTION; leave empty for roaming providers.";
            };
          };
        }));
      };
    };

    extraPackages = lib.mkOption {
      type = lib.types.listOf lib.types.package;
      default = [ ];
      description = "Additional packages made available to zkad, such as the host's ntfy-send package.";
    };

    attention.states = lib.mkOption {
      type = lib.types.nonEmptyListOf (lib.types.enum [ "blocked" "error" "done" ]);
      default = [ "blocked" "error" "done" ];
      description = "Agent states included in the live attention surface.";
    };

    notifications.desktopEnabled = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = "Whether newly actionable panes and credential-use warnings try the local desktop channel; private-key operations require successful ntfy fallback when this is disabled or unavailable.";
    };

    notifications.ntfyEnabled = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = "Whether newly actionable local panes and best-effort refusal notices are sent through ntfy; credential-use fallback is mandatory and ignores this switch.";
    };

    notifications.ntfyIncludeEvidence = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = "Whether ntfy payloads include raw agent evidence such as assistant output and tool descriptions.";
    };

    notifications.ntfyCommand = lib.mkOption {
      type = lib.types.str;
      default = "ntfy-send";
      description = "ntfy helper executable name, resolved on zkad's PATH unless ntfyPackage pins it.";
    };

    notifications.ntfyPackage = lib.mkOption {
      type = lib.types.nullOr lib.types.package;
      default = null;
      description = ''
        Package providing the ntfy helper named by notifications.ntfyCommand.
        When set, the command is written into the runtime configuration as an
        absolute path inside this package and the package joins zkad's PATH.
        On a headless origin ntfy is the only notification channel that can
        reach you, so leave this null there only if extraPackages already
        supplies the helper.
      '';
    };

    linger.users = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      example = [ "felix" ];
      description = ''
        Users whose systemd user instance lingers (users.users.<name>.linger)
        so zkad and /run/user/UID survive SSH logout. Without lingering on a
        headless origin, agent hooks silently no-op and notifications stop
        the moment the last SSH session closes. Listed users must be defined
        elsewhere in the configuration.
      '';
    };

    codex = {
      enableManagedHooks = lib.mkOption {
        type = lib.types.bool;
        default = true;
        description = "Install zka lifecycle hooks in the system Codex requirements file.";
      };

      extraRequirements = lib.mkOption {
        type = lib.types.attrs;
        default = { };
        description = "Additional values rendered into /etc/codex/requirements.toml.";
      };
    };

    claude.enableManagedHooks = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = "Install zka lifecycle hooks in the system Claude Code managed-settings drop-in directory.";
    };
  };

  config = lib.mkIf cfg.enable (lib.mkMerge [
    {
      assertions = [
        {
          assertion = !cfg.codex.enableManagedHooks || lib.all (path: !(lib.hasAttrByPath path cfg.codex.extraRequirements)) reservedRequirementPaths;
          message = "services.zka.codex.extraRequirements must not override zka-managed hook keys";
        }
        {
          assertion = cfg.notifications.ntfyPackage == null || !lib.hasPrefix "/" cfg.notifications.ntfyCommand;
          message = "services.zka.notifications.ntfyCommand must be a bare name when ntfyPackage is set";
        }
        {
          assertion = !forbiddenForwardAgent;
          message = "services.zka.ssh options must not enable ForwardAgent; use a credential bundle with sshAgent.enable";
        }
        {
          assertion = cfg.credentials.defaultBundle == null || builtins.hasAttr cfg.credentials.defaultBundle cfg.credentials.bundles;
          message = "services.zka.credentials.defaultBundle must name a configured credential bundle";
        }
        {
          assertion = cfg.credentials.pivb.forwardSocket == null || lib.hasPrefix "/" cfg.credentials.pivb.forwardSocket;
          message = "services.zka.credentials.pivb.forwardSocket must be absolute";
        }
      ] ++ credentialBundleAssertions ++ credentialProviderAssertions;

      warnings =
        lib.optional (cfg.headless && cfg.notifications.ntfyEnabled && cfg.notifications.ntfyPackage == null)
          "services.zka: headless origin with ntfy enabled but no notifications.ntfyPackage; unless extraPackages supplies the helper, no notification channel will reach you"
        ++ lib.optional (cfg.headless && cfg.linger.users == [ ])
          "services.zka: headless origin without services.zka.linger.users; zkad stops with your last SSH session unless lingering is managed elsewhere"
        ++ lib.optional (anyOpenPGPBundle && cfg.notifications.ntfyPackage == null)
          "services.zka: OpenPGP credential use fails closed when desktop delivery fails and ntfy-send is unavailable; set notifications.ntfyPackage or provide the helper through extraPackages"
        ++ lib.optional cfg.credentials.gnupg.configureAgent
          "services.zka.credentials.gnupg.configureAgent takes ownership of the globally shared user gpg-agent and extra socket; coordinate with other smart-card consumers";

      users.users = lib.genAttrs cfg.linger.users (_: { linger = true; });

      environment.systemPackages = servicePath;
      environment.sessionVariables.ZKA_CONFIG = runtimeConfig;

      systemd.user.services.zkad = {
        description = "zka Kitty workspace daemon";
        wantedBy = [ "default.target" ];
        path = servicePath;
        environment.ZKA_CONFIG = runtimeConfig;
        serviceConfig = {
          ExecStart = "${cfg.package}/bin/zka daemon";
          Restart = "on-failure";
          RestartSec = 1;
          TimeoutStopSec = 15;
          UMask = "0077";
          NoNewPrivileges = true;
        };
      };

      programs.gnupg.agent = lib.mkIf cfg.credentials.gnupg.configureAgent {
        enable = true;
        enableExtraSocket = true;
        pinentryPackage = cfg.credentials.gnupg.pinentryPackage;
      };
    }

    (lib.mkIf cfg.headless {
      # The desktop channel needs a session bus and a local Kitty view;
      # neither ever exists here. mkDefault keeps an explicit override
      # possible.
      services.zka.notifications.desktopEnabled = lib.mkDefault false;
    })

    (lib.mkIf cfg.codex.enableManagedHooks {
      environment.etc."codex/requirements.toml".source = requirementsFile;
    })

    (lib.mkIf cfg.claude.enableManagedHooks {
      environment.etc."claude-code/managed-settings.d/50-zka.json".source = claudeManagedSettingsFile;
    })
  ]);
}
