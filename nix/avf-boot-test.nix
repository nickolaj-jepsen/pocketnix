# Boots an AVF disk image (avf.enable on, unlike test.nix) in the build
# sandbox, in `qemu-machine.nix`'s machine, through the hvc0 autologin: the
# system must reach `running` with zram swap on, and a real `switch` must
# install the boot loader, as every update on the phone does. TCG only, so it
# runs anywhere. `avf-boot` boots the example's image; `base-boot` boots the
# base image, the managed guest the app's setup downloads, whose autologin is
# root. `prompt` is a Tcl regex matching the autologin's shell prompt.
{
  lib,
  runCommand,
  callPackage,
  expect,
  name,
  image,
  prompt,
}:
runCommand "pocketnix-${name}" {nativeBuildInputs = [expect];} ''
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
  spawn ${lib.getExe (callPackage ./qemu-machine.nix {})} ${image} . \
    -smp 2 -chardev stdio,id=con,signal=off -monitor none
  expect {
    -re {${prompt}} {}
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
