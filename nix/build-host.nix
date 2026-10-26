# The build host's half of pocketnix updates: a dedicated, untrusted user
# whose ssh key per guest may only build that guest's system and serve the
# copy, so a stolen phone gets no shell on the desktop.
{
  config,
  lib,
  pkgs,
  ...
}: let
  inherit (lib) mkOption types;
  cfg = config.pocketnix.buildHost;

  # The agent asks for two things: the build (whose installable is ignored
  # for the pinned one) and, for `nix copy --from ssh-ng://`, a daemon.
  helper = pkgs.writeShellApplication {
    name = "pocketnix-build-host";
    runtimeInputs = [config.nix.package];
    text = ''
      installable=$1
      case "''${SSH_ORIGINAL_COMMAND:-}" in
        "nix-daemon --stdio")
          exec nix-daemon --stdio ;;
        "nix build "* | "nix "*" build "*)
          exec nix --extra-experimental-features 'nix-command flakes' build --no-link --print-out-paths --refresh "$installable" ;;
        *)
          echo "pocketnix-build-host: only builds $installable; refusing: ''${SSH_ORIGINAL_COMMAND:-a shell}" >&2
          exit 1 ;;
      esac
    '';
  };
in {
  options.pocketnix.buildHost = {
    enable = lib.mkEnableOption "serving pocketnix guests' updates as a build host";
    user = mkOption {
      type = types.str;
      default = "pocketnix-build";
      description = ''
        The account guests ssh to, as `pocketnix.guest.updates.buildHost.host`
        (`<user>@<this host>`). It isn't in `nix.settings.trusted-users`, and
        its keys run nothing but the build and the copy.
      '';
    };
    guests = mkOption {
      default = {};
      description = "Guests allowed to build here, each pinned to one installable.";
      type = types.attrsOf (types.submodule {
        options = {
          publicKey = mkOption {
            type = types.str;
            example = "ssh-ed25519 AAAA… root@phone";
            description = ''
              The guest's ssh public key: that of `pocketnix.guest.updates.buildHost.sshKey`,
              by default its sshd host key (`/etc/ssh/ssh_host_ed25519_key.pub`).
            '';
          };
          installable = mkOption {
            type = types.str;
            example = "github:alice/nixos#nixosConfigurations.phone.config.system.build.toplevel";
            description = ''
              The only thing this guest's key builds, whatever it asks for: the
              guest's own `updates.buildHost.installable`.
            '';
          };
        };
      });
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = !(lib.elem cfg.user config.nix.settings.trusted-users);
        message = "pocketnix.buildHost.user must not be a trusted Nix user: a trusted daemon client can import unsigned paths";
      }
    ];
    users.users.${cfg.user} = {
      isSystemUser = true;
      group = cfg.user;
      # sshd runs a forced command through the login shell; Nix wants a home for its caches.
      useDefaultShell = true;
      home = "/var/lib/${cfg.user}";
      createHome = true;
      openssh.authorizedKeys.keys =
        lib.mapAttrsToList (_: g: ''restrict,command="${lib.getExe helper} ${lib.escapeShellArg g.installable}" ${g.publicKey}'') cfg.guests;
    };
    users.groups.${cfg.user} = {};
  };
}
