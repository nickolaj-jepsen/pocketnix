# What a NixOS guest provides to the pocketnix app: a login on the hvc0
# console (the recovery tab) and the agent the app dials over vsock.
{self}: {
  config,
  lib,
  pkgs,
  ...
}: let
  cfg = config.pocketnix.guest;
in {
  options.pocketnix.guest = {
    enable = lib.mkEnableOption "the pocketnix guest side";
    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.pocketnix;
      defaultText = lib.literalExpression "pocketnix.packages.\${system}.pocketnix";
      description = "The pocketnix package providing `pocketnix-agent`.";
    };
  };

  config = lib.mkIf cfg.enable {
    boot.kernelParams = ["console=hvc0"];
    boot.kernelModules = ["vmw_vsock_virtio_transport"];

    # The app's terminal speaks xterm, not serial-getty's default vt220.
    systemd.services."serial-getty@hvc0".environment.TERM = "xterm-256color";

    systemd.services.pocketnix-agent = {
      description = "pocketnix guest agent";
      wantedBy = ["multi-user.target"];
      after = ["systemd-modules-load.service"];
      serviceConfig = {
        ExecStart = "${cfg.package}/bin/pocketnix-agent";
        Restart = "always";
        RestartSec = 1;
        # The app's only way into a wedged guest besides the console.
        OOMScoreAdjust = -1000;
      };
    };
  };
}
