{pkgs}: let
  android = import ./android.nix {inherit pkgs;};
  goTests = pkgs.callPackage ./go.nix {withTests = true;};
  compositorTests = pkgs.callPackage ./compositor.nix {withTests = true;};
  # This checkout's CLI, rebuilt on each run (Go caches it) and run from the caller's directory.
  pocketnix = pkgs.writeShellApplication {
    name = "pocketnix";
    runtimeInputs = [pkgs.git pkgs.go];
    text = ''
      root=$(git rev-parse --show-toplevel)
      bin=''${XDG_CACHE_HOME:-$HOME/.cache}/pocketnix-dev/pocketnix
      go -C "$root/go" build -o "$bin" ./cmd/pocketnix
      exec "$bin" "$@"
    '';
  };
in
  pkgs.mkShell (android.env
    // {
      # The Go and compositor checks' build and test inputs.
      inputsFrom = [goTests compositorTests];
      packages = android.packages ++ [pkgs.gopls pkgs.clippy pkgs.rust-analyzer pocketnix];
      RUST_SRC_PATH = pkgs.rustPlatform.rustLibSrc;
      # `cargo test` runs the GUI tests against foot as the check does.
      inherit (compositorTests) FOOT FONTCONFIG_FILE;
    })
