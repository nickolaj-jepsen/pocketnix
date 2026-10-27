{
  lib,
  rustPlatform,
  pkg-config,
  libxkbcommon,
  foot,
  makeFontsConf,
  dejavu_fonts,
}:
rustPlatform.buildRustPackage {
  pname = "pocketnix-compositor";
  version = lib.fileContents ../VERSION;
  src = lib.cleanSourceWith {
    name = "pocketnix-compositor";
    src = ../compositor;
  };
  cargoLock.lockFile = ../compositor/Cargo.lock;

  nativeBuildInputs = [pkg-config];
  buildInputs = [libxkbcommon];

  # tests/gui.rs runs foot against the compositor when FOOT is set.
  nativeCheckInputs = [foot];
  FOOT = lib.getExe foot;
  FONTCONFIG_FILE = makeFontsConf {fontDirectories = [dejavu_fonts];};
  # Each test starts its own compositor; one at a time keeps their logs apart.
  checkFlags = ["--test-threads=1"];

  meta = {
    description = "pocketnix's guest Wayland compositor, serving windows to the app";
    license = lib.licenses.asl20;
    mainProgram = "pocketnix-compositor";
    platforms = lib.platforms.linux;
  };
}
