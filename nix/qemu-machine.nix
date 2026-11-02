# The QEMU machine that comes closest to crosvm for an AVF disk image:
# qemu-system-aarch64 (TCG), edk2 instead of u-boot, hvc0 on a virtconsole.
# `pocketnix-qemu <image> <dir> <qemu args…>` puts a writable copy of the
# image's disk and the firmware's variables in <dir> and runs QEMU on them.
# The caller adds `-smp`, the chardev `con` behind hvc0, and any monitor or
# vsock.
{
  writeShellApplication,
  qemu,
  coreutils,
}:
writeShellApplication {
  name = "pocketnix-qemu";
  runtimeInputs = [qemu coreutils];
  text = ''
    # A library path leaked from elsewhere can pull a newer glibc's libraries into QEMU.
    unset LD_LIBRARY_PATH
    image=$1 dir=$2
    shift 2
    # A writable sparse copy, grown so first boot's partition grow has room.
    cp --sparse=always "$image"/*.raw "$dir/disk.img"
    chmod u+w "$dir/disk.img"
    truncate -s 8G "$dir/disk.img"
    truncate -s 64M "$dir/vars.fd"
    # acpi=off: Google's kernel has no ACPI, so edk2 must hand it QEMU's device tree.
    exec qemu-system-aarch64 \
      -M virt,acpi=off -cpu max -m 2048 \
      -drive if=pflash,format=raw,readonly=on,file=${qemu}/share/qemu/edk2-aarch64-code.fd \
      -drive if=pflash,format=raw,file="$dir/vars.fd" \
      -drive if=none,id=disk,file="$dir/disk.img",format=raw \
      -device virtio-blk-pci,drive=disk \
      -device virtio-serial-pci \
      -device virtconsole,chardev=con \
      -nic user,model=virtio-net-pci \
      -display none -serial null \
      "$@"
  '';
}
