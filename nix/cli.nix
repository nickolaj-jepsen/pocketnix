# The host-side CLI with what `provision` runs on PATH; the guest's closure
# keeps the bare package.
{
  lib,
  symlinkJoin,
  makeWrapper,
  android-tools,
  e2fsprogs,
  pocketnix,
}:
symlinkJoin {
  name = "pocketnix-cli-${pocketnix.version}";
  paths = [pocketnix];
  nativeBuildInputs = [makeWrapper];
  postBuild = ''
    rm $out/bin/pocketnix-agent
    wrapProgram $out/bin/pocketnix --prefix PATH : ${lib.makeBinPath [android-tools e2fsprogs]}
  '';
  inherit (pocketnix) meta;
}
