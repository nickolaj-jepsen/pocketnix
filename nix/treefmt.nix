# `nix fmt` and checks.format: one config for both.
{pkgs}:
pkgs.treefmt.withConfig {
  runtimeInputs = [pkgs.alejandra pkgs.go pkgs.ktlint];
  settings = {
    tree-root-file = "flake.nix";
    # The compositor isn't rustfmt'ed; the rest is vendored.
    excludes = ["compositor/*" "terminal-emulator/*" "terminal-view/*" "app/src/main/cpp/*"];
    on-unmatched = "debug";
    formatter = {
      nix = {
        command = "alejandra";
        options = ["--quiet"];
        includes = ["*.nix"];
      };
      go = {
        command = "gofmt";
        options = ["-w"];
        includes = ["*.go"];
      };
      # Its rules are in .editorconfig.
      kotlin = {
        command = "ktlint";
        options = ["--format" "--log-level=error"];
        includes = ["*.kt" "*.kts"];
      };
    };
  };
}
