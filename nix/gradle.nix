# `nix run .#gradle -- <tasks>`: ./gradlew with the devshell's Android setup,
# and the real agent, compositor and foot that the JVM integration tests opt into.
{
  pkgs,
  pocketnix,
  pocketnix-compositor,
}: let
  android = import ./android.nix {inherit pkgs;};
  compositorTests = pkgs.callPackage ./compositor.nix {withTests = true;};
in
  pkgs.writeShellApplication {
    name = "gradle";
    runtimeInputs = android.packages ++ [pkgs.git pkgs.foot];
    runtimeEnv =
      android.env
      // {
        POCKETNIX_AGENT = pkgs.lib.getExe' pocketnix "pocketnix-agent";
        POCKETNIX_COMPOSITOR = pkgs.lib.getExe pocketnix-compositor;
        inherit (compositorTests) FONTCONFIG_FILE;
      };
    text = ''
      cd "$(git rev-parse --show-toplevel)"
      exec ./gradlew "$@"
    '';
  }
