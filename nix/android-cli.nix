# The Android CLI (`android`), for Android device streaming's `device remote`.
# Its launcher downloads the real CLI into ~/.android on first run, and both
# expect /lib64's loader: a bubblewrap FHS runs them as they are, because the
# Android SDK terms they ship under forbid modifying them or redistributing
# them. So keep this out of every check: CI's Cachix push would publish it.
{pkgs}: let
  version = "1.1.16717390";
  launcher = pkgs.fetchurl {
    url = "https://dl.google.com/android/cli/${version}/linux_x86_64/android";
    hash = "sha256-NnRJKtV2FeX6rPBFINrxtbBPqfoFJH207EP4kKG0szQ=";
    executable = true;
  };
in
  pkgs.buildFHSEnv {
    pname = "android";
    inherit version;
    # secret-tool, where `android auth login` keeps its token.
    targetPkgs = p: [p.libsecret];
    runScript = launcher;
    meta.license = pkgs.lib.licenses.unfree;
  }
