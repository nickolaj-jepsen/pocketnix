# The host-side CLI with what `provision` runs on PATH.
{
  lib,
  callPackage,
  symlinkJoin,
  makeWrapper,
  android-tools,
  e2fsprogs,
}: let
  unwrapped = callPackage ./go.nix {
    pname = "pocketnix-cli";
    subPackages = ["cmd/pocketnix"];
  };
in
  symlinkJoin {
    name = "pocketnix-cli-${unwrapped.version}";
    paths = [unwrapped];
    nativeBuildInputs = [makeWrapper];
    postBuild = ''
      wrapProgram $out/bin/pocketnix --prefix PATH : ${lib.makeBinPath [android-tools e2fsprogs]}
    '';
    inherit (unwrapped) meta;
  }
