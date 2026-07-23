{ lib
, buildGoModule
, makeWrapper
, alsa-lib
, libpulseaudio
, whisper-cpp
, piper-tts
, rev ? "dev"
}:

buildGoModule {
  pname = "aispeech";
  version = "0.0.1-${rev}";

  src = lib.cleanSource ./.;

  # Changes only when go.mod dependencies change. On mismatch, nix prints the
  # correct value — replace it and rebuild.
  vendorHash = "sha256-i8whWvIg7+UiEfyZofY1zz102V7V/a7y4dg6htqtX7U=";

  # cgo: malgo compiles miniaudio, which dlopens ALSA/PulseAudio at runtime.
  nativeBuildInputs = [ makeWrapper ];
  buildInputs = [ alsa-lib libpulseaudio ];

  subPackages = [ "cmd/aispeech" ];
  ldflags = [ "-s" "-w" ];

  # Put the speech engines on PATH and make the dlopen'd audio libs findable.
  postInstall = ''
    wrapProgram $out/bin/aispeech \
      --prefix PATH : ${lib.makeBinPath [ whisper-cpp piper-tts ]} \
      --prefix LD_LIBRARY_PATH : ${lib.makeLibraryPath [ alsa-lib libpulseaudio ]}
  '';

  meta = with lib; {
    description = "Local voice hub: gives terminal AI agents speak() and listen() over MCP";
    homepage = "https://github.com/mkrzywonski/aispeech";
    license = licenses.gpl3Only;
    platforms = platforms.linux;
    mainProgram = "aispeech";
  };
}
