{
  lib,
  buildGoModule,
}:
buildGoModule {
  pname = "pocketnix";
  version = "0.0.0";
  # Unpacked as plain "go", the source would land on the builder's GOPATH (/build/go).
  src = lib.cleanSourceWith {
    name = "pocketnix-go";
    src = ../go;
  };
  vendorHash = "sha256-ARPAhSApRRJ6ufKKxlMZLLVP4M+y/nXdlxwZdAHM92E=";

  # The agent runs in the guest from a NixOS store path; no cgo keeps it a single static binary.
  env.CGO_ENABLED = 0;

  meta = {
    description = "pocketnix guest agent and host-side CLI";
    license = lib.licenses.gpl3Plus;
    mainProgram = "pocketnix";
    platforms = lib.platforms.linux;
  };
}
