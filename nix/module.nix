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
  hookCommand = "${cfg.package}/libexec/zka/hooks/zka hook codex";
  hook = {
    hooks = [
      {
        type = "command";
        command = hookCommand;
        timeout = 2;
      }
    ];
  };
  zkaRequirements = {
    features.hooks = true;
    hooks = {
      managed_dir = "${cfg.package}/libexec/zka/hooks";
      SessionStart = [
        (hook // { matcher = "startup|resume|clear|compact"; })
      ];
      UserPromptSubmit = [ hook ];
      PermissionRequest = [
        (hook // { matcher = ".*"; })
      ];
      PostToolUse = [
        (hook // { matcher = ".*"; })
      ];
      Stop = [ hook ];
    };
  };
  requirements = lib.recursiveUpdate cfg.codex.extraRequirements zkaRequirements;
  requirementsFile = toml.generate "zka-codex-requirements.toml" requirements;
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
  runtimeConfig = json.generate "zka-config.json" {
    shell.command = cfg.shell.command;
    kitty = {
      command = "${cfg.kitty.package}/bin/kitty";
      kitten_command = "${cfg.kitty.package}/bin/kitten";
      watcher = toString cfg.kitty.watcher;
      extra_args = cfg.kitty.extraArgs;
    };
    zmx.command = zmxCommand;
    ssh = {
      command = "${cfg.ssh.package}/bin/ssh";
      options = cfg.ssh.options;
    };
    notifications.ntfy_command = cfg.notifications.ntfyCommand;
  };
  servicePath = [
    cfg.package
    cfg.shell.package
    cfg.kitty.package
    cfg.ssh.package
  ]
  ++ lib.optional (cfg.zmx.package != null) cfg.zmx.package
  ++ cfg.extraPackages;
in
{
  options.services.zka = {
    enable = lib.mkEnableOption "zka Kitty workspace orchestration";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
      defaultText = lib.literalExpression "self.packages.\${pkgs.stdenv.hostPlatform.system}.default";
      description = "The zka package to run.";
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

    ssh = {
      package = lib.mkPackageOption pkgs "openssh" { };

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
    };

    extraPackages = lib.mkOption {
      type = lib.types.listOf lib.types.package;
      default = [ ];
      description = "Additional packages made available to zkad, such as the host's ntfy-send package.";
    };

    notifications.ntfyCommand = lib.mkOption {
      type = lib.types.str;
      default = "ntfy-send";
      description = "ntfy helper executable name or absolute path.";
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
  };

  config = lib.mkIf cfg.enable (lib.mkMerge [
    {
      assertions = [
        {
          assertion = !cfg.codex.enableManagedHooks || lib.all (path: !(lib.hasAttrByPath path cfg.codex.extraRequirements)) reservedRequirementPaths;
          message = "services.zka.codex.extraRequirements must not override zka-managed hook keys";
        }
      ];

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
    }

    (lib.mkIf cfg.codex.enableManagedHooks {
      environment.etc."codex/requirements.toml".source = requirementsFile;
    })
  ]);
}
