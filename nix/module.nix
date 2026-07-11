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
  # zka's hook and feature keys are reserved; its values win on collision.
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
in
{
  options.services.zka = {
    enable = lib.mkEnableOption "zka persistent coding-agent orchestration";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
      defaultText = lib.literalExpression "self.packages.\${pkgs.stdenv.hostPlatform.system}.default";
      description = "The zka package to run.";
    };

    kittyPackage = lib.mkPackageOption pkgs "kitty" { };

    zmxPackage = lib.mkOption {
      type = lib.types.nullOr lib.types.package;
      default = null;
      description = "Optional zmx package added to the user service PATH; leave null when supplied system-wide.";
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

      environment.systemPackages =
        [
          cfg.package
          cfg.kittyPackage
        ]
        ++ lib.optional (cfg.zmxPackage != null) cfg.zmxPackage
        ++ cfg.extraPackages;

      systemd.user.services.zkad = {
        description = "zka coding-agent session daemon";
        wantedBy = [ "default.target" ];
        path =
          [
            cfg.package
            cfg.kittyPackage
          ]
          ++ lib.optional (cfg.zmxPackage != null) cfg.zmxPackage
          ++ cfg.extraPackages;
        environment = {
          ZKA_NTFY_COMMAND = cfg.notifications.ntfyCommand;
        };
        serviceConfig = {
          ExecStart = "${cfg.package}/bin/zka daemon";
          Restart = "on-failure";
          RestartSec = 1;
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
