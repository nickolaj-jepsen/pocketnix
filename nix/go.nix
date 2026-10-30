# One Go module, built per use: by default the guest's binaries, from a
# source without the tests or the other commands, so neither rebuilds the
# guest's image; `withTests` vets and tests the whole module instead.
{
  lib,
  buildGoModule,
  llvmPackages,
  e2fsprogs,
  dbus,
  pname ? "pocketnix",
  subPackages ? ["cmd/pocketnix-agent" "cmd/pocketnix-guest"],
  withTests ? false,
}: let
  otherCommands = lib.filter (c: !lib.elem "cmd/${c}" subPackages) (lib.attrNames (builtins.readDir ../go/cmd));
in
  buildGoModule (finalAttrs: {
    inherit pname;
    # The app shares it; a git rev here would rebuild the agent, and so the image, on every commit.
    version = lib.fileContents ../VERSION;
    src = lib.fileset.toSource {
      root = ../go;
      fileset =
        if withTests
        then ../go
        else
          lib.fileset.difference ../go (lib.fileset.unions (
            [(lib.fileset.fileFilter (f: lib.hasSuffix "_test.go" f.name) ../go)]
            ++ map (c: ../go/cmd + "/${c}") otherCommands
          ));
    };
    subPackages = lib.optionals (!withTests) subPackages;
    # A module cache rather than vendor/, whose contents follow the source's imports: every
    # variant shares this hash.
    proxyVendor = true;
    vendorHash = "sha256-4lP2HSoobu81vk4kTDxnmDNyiBy1KvIbNl9YBhSBOBI=";

    # The agent runs in the guest from a NixOS store path; no cgo keeps it a single static binary.
    env.CGO_ENABLED = 0;
    # bpf2go's clang and llvm-strip. Unwrapped: the wrapper's hardening flags aren't valid for BPF,
    # and its cross-compiling name is target-prefixed.
    nativeBuildInputs = [llvmPackages.clang-unwrapped llvmPackages.llvm];
    # The BPF object is git-ignored; GOOS/GOARCH cleared so the generator runs on the build platform.
    preBuild = "GOOS= GOARCH= go generate ./internal/portwatch";
    doCheck = withTests;
    # The --inject tests edit a real ext4 image; notifyd's run a private session bus.
    nativeCheckInputs = [e2fsprogs dbus];
    preCheck = "go vet ./...";
    ldflags = [
      "-s"
      "-X main.version=${finalAttrs.version}"
    ];

    meta = {
      description = "pocketnix guest agent and host-side CLI";
      license = lib.licenses.gpl3Plus;
      mainProgram = baseNameOf (lib.head subPackages);
      platforms = lib.platforms.linux;
    };
  })
