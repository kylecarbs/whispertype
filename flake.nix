{
  description = "WhisperType with whisper-cpp-server and systray client";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    let
      # Define the NixOS module independently of system
      nixosModule = { config, lib, pkgs, ... }: {
        options.services.whispertype = {
          enable = lib.mkEnableOption "WhisperType service";
          port = lib.mkOption {
            type = lib.types.int;
            default = 36124;
            description = "Port for whisper-cpp-server";
          };
          host = lib.mkOption {
            type = lib.types.str;
            default = "localhost";
            description = "Host for whisper-cpp-server";
          };
          package = lib.mkOption {
            type = lib.types.package;
            description = "The WhisperType package to use";
            default = self.packages.${pkgs.system}.default;
          };
        };

        config = lib.mkIf config.services.whispertype.enable {
          # The backend server service
          systemd.services.whisper-cpp-server = {
            description = "Whisper CPP Server for WhisperType";
            wantedBy = [ "multi-user.target" ];
            after = [ "network.target" ];

            serviceConfig = {
              ExecStart = "${config.services.whispertype.package}/bin/whispertype-server";
              Restart = "always";
              RestartSec = "10";
              Type = "simple";
            };
          };

          # The systray client service (runs per-user)
          systemd.user.services.whispertype-client = {
            description = "WhisperType Client";
            wantedBy = [ "graphical-session.target" ];
            partOf = [ "graphical-session.target" ];
            after = [ "graphical-session.target" "graphical-session-pre.target" ];
            
            environment = {
              DISPLAY = ":0";
              XAUTHORITY = "${config.users.users.kyle.home}/.Xauthority";
            };

            path = [ pkgs.pulseaudio ];

            serviceConfig = {
              ExecStart = ''
                ${config.services.whispertype.package}/bin/whispertype \
                  -host ${config.services.whispertype.host} \
                  -port ${toString config.services.whispertype.port}
              '';
              Restart = "always";
              RestartSec = "10";
              Type = "simple";
              
              # Add these for better service management
              RemainAfterExit = true;
              StartLimitIntervalSec = 0;
            };
          };

          # Ensure the required packages are available
          environment.systemPackages = [
            config.services.whispertype.package
          ];
        };
      };
    in
    (flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs {
          inherit system;
          config.allowUnfree = true;
        };

        # Download the model during build
        whisperModel = pkgs.fetchurl {
          url = "https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-base.en.bin";
          sha256 = "sha256-oDd5yG3zMjB19eeWyyzlAp8A7Ihp7uP9+4l6/jbG0AI="; # You'll need to replace this
        };

        # Create wrapped commands
        whispertype-server = let
          whisper-cpp = pkgs.openai-whisper-cpp.override { cudaSupport = true; };
        in pkgs.writeShellScriptBin "whispertype-server" ''
          exec ${whisper-cpp}/bin/whisper-cpp-server \
            -m ${whisperModel} \
            --port 36124 \
            "$@"
        '';

        # Build the Go client
        whispertype-client = pkgs.buildGoModule {
          pname = "whispertype";
          version = "0.1.0";
          src = ./.;

          vendorHash = "sha256-UBWaJbHu6llpGBsbF4blY9mSyXU8xkx5voTpYjPAAxk=";
          proxyVendor = false;
          doCheck = false;

          buildInputs = with pkgs; [
            pulseaudio
            libayatana-appindicator
            gtk3
          ];

          nativeBuildInputs = with pkgs; [
            pkg-config
            go_1_23
          ];

          # Ensure pulseaudio is available at runtime
          nativeCheckInputs = [ pkgs.pulseaudio ];
          LD_LIBRARY_PATH = pkgs.lib.makeLibraryPath [ pkgs.pulseaudio ];
        };

        # Combined package with both client and server
        whispertype = pkgs.symlinkJoin {
          name = "whispertype";
          paths = [ whispertype-client whispertype-server ];
        };
      in
      {
        packages = {
          inherit whispertype-client whispertype-server whispertype;
          default = whispertype;
        };

        # Development shell
        devShells.default = pkgs.mkShell {
          buildInputs = with pkgs; [
            # For the client
            go_1_23
            pkg-config
            pulseaudio
            libayatana-appindicator
            gtk3

            # For the server
            openai-whisper-cpp

            # Our wrapped commands
            whispertype
          ];

          # Ensure pulseaudio is available
          LD_LIBRARY_PATH = pkgs.lib.makeLibraryPath [ pkgs.pulseaudio ];
        };
      }
    )) // {
      # Make the NixOS module available at the top level
      nixosModules.default = nixosModule;
      # Also expose the module directly
      inherit nixosModule;
    };
} 