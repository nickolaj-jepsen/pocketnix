{
  lib,
  buildGoModule,
}:
buildGoModule (finalAttrs: {
  pname = "pocketnix";
  version = "0.0.0";
  # Unpacked as plain "go", the source would land on the builder's GOPATH (/build/go).
  src = lib.cleanSourceWith {
    name = "pocketnix-go";
    src = ../go;
  };
  vendorHash = "sha256-mpY15mzV8qNGMLofA3jspNbsCK+oEXHYTNTk+FQjrLo=";

  # The agent runs in the guest from a NixOS store path; no cgo keeps it a single static binary.
  env.CGO_ENABLED = 0;
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
