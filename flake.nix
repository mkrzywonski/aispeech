{
  description = "aispeech — local voice hub for terminal AI agents";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-25.11";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" ];
      forAll = f: nixpkgs.lib.genAttrs systems f;
      rev = self.shortRev or self.dirtyShortRev or "dev";
    in
    {
      overlays.default = final: prev: {
        aispeech = final.callPackage ./package.nix { inherit rev; };
      };

      packages = forAll (system:
        let
          pkgs = import nixpkgs { inherit system; overlays = [ self.overlays.default ]; };
        in
        {
          default = pkgs.aispeech;
          aispeech = pkgs.aispeech;
        });

      devShells = forAll (system:
        let pkgs = import nixpkgs { inherit system; };
        in {
          default = pkgs.mkShell {
            # cgo (malgo) + the runtime speech engines for `go run`/`go test`.
            packages = [
              pkgs.go
              pkgs.gcc
              pkgs.pkg-config
              pkgs.alsa-lib
              pkgs.libpulseaudio
              pkgs.whisper-cpp
              pkgs.piper-tts
            ];
            # miniaudio dlopens these at runtime; put them on the loader path so
            # `go run ./cmd/aispeech` sees real ALSA/PulseAudio devices.
            LD_LIBRARY_PATH = pkgs.lib.makeLibraryPath [ pkgs.alsa-lib pkgs.libpulseaudio ];
          };
        });
    };
}
