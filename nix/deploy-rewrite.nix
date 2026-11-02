# `pocketnix deploy compositor`'s copy of a cross-built package: its binaries
# load a guest's own loader and libraries, store paths that exist only in that
# guest, so the copy holds this output alone instead of the cross closure.
{
  lib,
  runCommand,
  patchelf,
  # The cross-built package, with its binaries in bin/.
  package,
  # The guest's dynamic loader, and the library directories the binaries need, in order.
  interpreter,
  rpath,
}:
runCommand "${package.pname}-guest-libs-${package.version}" {
  nativeBuildInputs = [patchelf];
  # The guest's paths aren't inputs, so nothing here may refer to a store path.
  allowedReferences = [];
} ''
  mkdir -p $out/bin
  for b in ${package}/bin/*; do
    install -m 0755 "$b" $out/bin/
    patchelf --set-interpreter ${lib.escapeShellArg interpreter} --set-rpath ${lib.escapeShellArg (lib.concatStringsSep ":" rpath)} "$out/bin/''${b##*/}"
  done
''
