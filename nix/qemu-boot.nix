# `nix run .#boot-example`: boots the example's AVF disk image on this machine
# in `qemu-machine.nix`'s machine, with the agent on vsock. Drive it with
# `pocketnix agent --connect vsock:<cid>:9100 …`; the console is on stdio.
{
  lib,
  writeShellApplication,
  callPackage,
  coreutils,
  image,
}:
writeShellApplication {
  name = "boot-example";
  runtimeInputs = [coreutils];
  text = ''
    cid=''${POCKETNIX_CID:-4242}
    dir=$(mktemp -d)
    trap 'rm -rf "$dir"' EXIT
    echo "agent: vsock:$cid:9100; console below (Ctrl-A X quits)" >&2
    ${lib.getExe (callPackage ./qemu-machine.nix {})} ${image} "$dir" \
      -smp 4 \
      -chardev stdio,id=con,mux=on,signal=off \
      -mon chardev=con,mode=readline \
      -device vhost-vsock-pci,guest-cid="$cid"
  '';
}
