{
  lib,
  stdenv,
  pkg-config,
  wayland,
  wayland-scanner,
  wlr-protocols,
}:
# The agent's in-process screencopy client (capture/pocketnix-capture.c). The
# wlr-screencopy protocol code is generated here, not committed.
stdenv.mkDerivation {
  pname = "pocketnix-capture";
  version = "0.1.0";
  src = ../capture;
  nativeBuildInputs = [pkg-config wayland-scanner];
  buildInputs = [wayland];

  buildPhase = ''
    runHook preBuild
    xml=${wlr-protocols}/share/wlr-protocols/unstable/wlr-screencopy-unstable-v1.xml
    wayland-scanner client-header "$xml" wlr-screencopy.h
    wayland-scanner private-code "$xml" wlr-screencopy.c
    $CC -O2 -Wall -o pocketnix-capture pocketnix-capture.c wlr-screencopy.c \
      $(pkg-config --cflags --libs wayland-client)
    runHook postBuild
  '';

  installPhase = ''
    runHook preInstall
    install -Dm755 pocketnix-capture $out/bin/pocketnix-capture
    runHook postInstall
  '';

  meta = {
    description = "wlr-screencopy client that prints damage rects and RGBA for the pocketnix agent";
    license = lib.licenses.gpl3Plus;
    platforms = lib.platforms.linux;
    mainProgram = "pocketnix-capture";
  };
}
