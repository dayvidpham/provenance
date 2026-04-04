{
  description = "Reproducible Go dev environment with build and test support";

  # ============================================================
  # INPUTS
  # ============================================================

  inputs = rec {
    nixpkgs-stable.url = "github:NixOS/nixpkgs/nixos-25.11";
    nixpkgs-unstable.url = "github:NixOS/nixpkgs/nixos-unstable";
    nixpkgs = nixpkgs-unstable;
    flake-utils.url = "github:numtide/flake-utils";
  };

  # ============================================================
  # OUTPUTS
  # ============================================================

  outputs =
    inputs@{ self
    , nixpkgs
    , nixpkgs-stable
    , nixpkgs-unstable
    , flake-utils
    , ...
    }:
    let
      # ==========================================================
      # PROJECT CONFIGURATION — edit this section for your project
      # ==========================================================

      # Package metadata
      pname = "provenance";
      version = "0.1.0";

      # Go package attribute — null uses default from nixpkgs
      goAttr = null;

      # Vendor hash for buildGoModule. No vendor/ dir, so Nix
      # downloads deps and verifies against this hash.
      # Run `nix build` once with lib.fakeHash to get the real hash.
      vendorHash = null; # TODO: replace with real hash from first `nix build`

      # CLI tools available in the dev shell
      devTools = pkgs: with pkgs; [
        gopls
        gotools              # goimports, godoc
        go-tools             # staticcheck
        delve
        ast-grep
      ];

      # No native build deps — pure Go, CGO_ENABLED=0
      nativeBuildDeps = pkgs: [ ];

      # Quality gates matching Makefile
      extraCheckPhase = ''
        go vet ./...
      '';

      # Library — no binary to install
      extraInstallPhase = ''
      '';

      # ==========================================================
      # IMPLEMENTATION — you shouldn't need to edit below here
      # ==========================================================

      mkOutputs = nixpkgs-channel:
        flake-utils.lib.eachDefaultSystem (system:
          let
            pkgs = import nixpkgs-channel {
              inherit system;
              config.allowUnfree = true;
            };

            goPackage = if goAttr != null
              then pkgs.${goAttr}
              else pkgs.go;

            # ----------------------------------------------------------
            # Build
            # ----------------------------------------------------------

            package = pkgs.buildGoModule {
              inherit pname version;
              src = ./.;
              inherit vendorHash;

              nativeBuildInputs = nativeBuildDeps pkgs;

              checkPhase = ''
                runHook preCheck
                CGO_ENABLED=0 go test -count=1 ./...
                CGO_ENABLED=0 go build ./...
                ${extraCheckPhase}
                runHook postCheck
              '';

              # Library package — no binary output
              postInstall = extraInstallPhase;
            };

            # ----------------------------------------------------------
            # Development Shell
            # ----------------------------------------------------------

            devShell = pkgs.mkShell {
              name = "${pname}-dev";
              inputsFrom = [ package ];
              packages = (devTools pkgs);

              shellHook = ''
                echo "Go $(go version | cut -d' ' -f3) dev shell"
              '';
            };

          in {
            packages.default = package;
            packages.${pname} = package;

            devShells.default = devShell;

            # Quick check: nix flake check
            checks.build = package;
          }
        );
    in
    mkOutputs nixpkgs;
}
