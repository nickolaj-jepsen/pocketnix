# `nix run .#dev-agent`: the example guest's agent on this machine, for a
# debug app on the Android emulator. It listens on loopback only, which the
# emulator reaches as 10.0.2.2: whoever reaches it gets a shell.
{
  pkgs,
  self,
}: let
  inherit (pkgs.stdenv.hostPlatform) system;
  # The example's config, evaluated for this machine so its store paths are this machine's.
  example = self.nixosConfigurations.example.extendModules {
    modules = [{nixpkgs.hostPlatform = pkgs.lib.mkForce system;}];
  };
  # Without `user`, sessions run as whoever runs the agent; without
  # `forwardOffers`, this machine's own listeners aren't offered.
  config = pkgs.runCommand "pocketnix-dev-agent.json" {nativeBuildInputs = [pkgs.jq];} ''
    jq 'del(.user, .forwardOffers)' ${example.config.environment.etc."pocketnix/agent.json".source} > $out
  '';
in
  pkgs.writeShellApplication {
    name = "dev-agent";
    runtimeInputs = [self.packages.${system}.pocketnix];
    text = ''
      port=''${POCKETNIX_PORT:-9100}
      echo "agent on tcp:127.0.0.1:$port; the emulator reaches it as 10.0.2.2:$port" >&2
      exec pocketnix-agent --config ${config} --listen "tcp:127.0.0.1:$port" --no-power "$@"
    '';
  }
