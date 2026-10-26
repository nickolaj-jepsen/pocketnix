# `nix run .#boot-example`: boots the example's AVF disk image on this machine
# under qemu-system-aarch64 (TCG), as close to crosvm as QEMU gets: edk2
# instead of u-boot, hvc0 on a virtconsole, the agent on vsock. Drive it with
# `pocketnix agent --connect vsock:<cid>:9100 …`; the console is on stdio.
{
  writeShellApplication,
  qemu,
  coreutils,
  image,
}:
writeShellApplication {
  name = "boot-example";
  runtimeInputs = [qemu coreutils];
  text = ''
    # A library path leaked from elsewhere can pull a newer glibc's libraries into QEMU.
    unset LD_LIBRARY_PATH
    cid=''${POCKETNIX_CID:-4242}
    dir=$(mktemp -d)
    trap 'rm -rf "$dir"' EXIT
    # A writable sparse copy, grown so first boot's partition grow has room.
    cp --sparse=always ${image}/*.raw "$dir/disk.img"
    chmod u+w "$dir/disk.img"
    truncate -s 8G "$dir/disk.img"
    truncate -s 64M "$dir/vars.fd"
    echo "agent: vsock:$cid:9100; console below (Ctrl-A X quits)" >&2
    # acpi=off: Google's kernel has no ACPI, so edk2 must hand it QEMU's device tree.
    qemu-system-aarch64 \
      -M virt,acpi=off -cpu max -smp 4 -m 2048 \
      -drive if=pflash,format=raw,readonly=on,file=${qemu}/share/qemu/edk2-aarch64-code.fd \
      -drive if=pflash,format=raw,file="$dir/vars.fd" \
      -drive if=none,id=disk,file="$dir/disk.img",format=raw \
      -device virtio-blk-pci,drive=disk \
      -device virtio-serial-pci \
      -chardev stdio,id=con,mux=on,signal=off \
      -device virtconsole,chardev=con \
      -mon chardev=con,mode=readline \
      -device vhost-vsock-pci,guest-cid="$cid" \
      -nic user,model=virtio-net-pci \
      -display none -serial null
  '';
}
