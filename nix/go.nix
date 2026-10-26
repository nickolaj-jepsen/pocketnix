{
  lib,
  buildGoModule,
  e2fsprogs,
  dbus,
}:
buildGoModule (finalAttrs: {
  pname = "pocketnix";
  # The app shares it; a git rev here would rebuild the agent, and so the image, on every commit.
  version = lib.fileContents ../VERSION;
  # Unpacked as plain "go", the source would land on the builder's GOPATH (/build/go).
  src = lib.cleanSourceWith {
    name = "pocketnix-go";
    src = ../go;
  };
  vendorHash = "sha256-RpQigkqATmLriMtDgkfC1b2irFEiXb/K13MZGodfNWI=";

  # The agent runs in the guest from a NixOS store path; no cgo keeps it a single static binary.
  env.CGO_ENABLED = 0;
  # The --inject tests edit a real ext4 image; notifyd's run a private session bus.
  nativeCheckInputs = [e2fsprogs dbus];
  ldflags = [
    "-s"
    "-X main.version=${finalAttrs.version}"
  ];

  meta = {
    description = "pocketnix guest agent and host-side CLI";
    license = lib.licenses.gpl3Plus;
    mainProgram = "pocketnix";
    platforms = lib.platforms.linux;
  };
})
