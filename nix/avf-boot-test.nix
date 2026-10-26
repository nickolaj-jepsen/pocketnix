# Boots the example's AVF disk image (avf.enable on, unlike test.nix) under
# qemu-system-aarch64 in the build sandbox, through the hvc0 autologin: the
# system must reach `running`, and a real `switch` must install the boot
# loader, as every update on the phone does. TCG only, so it runs anywhere.
{
  runCommand,
  qemu,
  expect,
  image,
}:
runCommand "pocketnix-avf-boot" {nativeBuildInputs = [qemu expect];} ''
  cp --sparse=always ${image}/*.raw disk.img
  chmod u+w disk.img
  # Room for first boot's partition grow.
  truncate -s 8G disk.img
  truncate -s 64M vars.fd
  cat > boot.exp <<'EOF'
  set timeout 1800
  # The console's lines end in \r, which Nix's log shows as empty: keep it in a file.
  log_file console.log
  proc step {cmd tag} {
    send "$cmd; echo $tag-\$?\r"
    expect {
      -re "$tag-(\[0-9\]+)" { if {$expect_out(1,string) ne "0"} { send_error "$tag failed\n"; exit 1 } }
      timeout { send_error "$tag timed out\n"; exit 1 }
    }
  }
  # acpi=off: Google's kernel has no ACPI, so edk2 must hand it QEMU's device tree.
  spawn qemu-system-aarch64 -M virt,acpi=off -cpu max -smp 2 -m 2048 \
    -drive if=pflash,format=raw,readonly=on,file=${qemu}/share/qemu/edk2-aarch64-code.fd \
    -drive if=pflash,format=raw,file=vars.fd \
    -drive if=none,id=disk,file=disk.img,format=raw -device virtio-blk-pci,drive=disk \
    -device virtio-serial-pci -chardev stdio,id=con,signal=off -device virtconsole,chardev=con \
    -nic user,model=virtio-net-pci -display none -serial null -monitor none
  # The autologin's prompt.
  expect {
    -re {@[^ ]+:~\]\$} {}
    timeout { send_error "no shell on hvc0\n"; exit 1 }
    eof { send_error "QEMU exited before a shell\n"; exit 1 }
  }
  step "sudo systemctl is-system-running --wait || { systemctl --failed --no-pager; false; }" STATE
  step "systemd-analyze --no-pager; systemd-analyze blame --no-pager | head -n 15" ANALYZE
  step "grep -q zram /proc/swaps" ZRAM
  step "sudo /run/current-system/bin/switch-to-configuration switch" SWITCH
  step "sudo test -e /boot/EFI/systemd/systemd-bootaa64.efi -a -e /boot/EFI/BOOT/BOOTAA64.EFI && sudo ls /boot/loader/entries" ESP
  send "sudo systemctl poweroff\r"
  expect eof
  EOF
  status=0
  expect boot.exp > /dev/null || status=$?
  tr -d '\r' < console.log | tail -n 80
  [ "$status" = 0 ] && touch $out
  exit "$status"
''
