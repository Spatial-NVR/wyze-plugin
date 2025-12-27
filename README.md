# Wyze Plugin for SpatialNVR

A plugin for integrating Wyze cameras with SpatialNVR. This plugin bundles [docker-wyze-bridge](https://github.com/mrlt8/docker-wyze-bridge) to provide local RTSP streaming from Wyze cameras.

## Features

- Wyze account authentication (including 2FA/TOTP)
- Automatic camera discovery from Wyze account
- **Bundled wyze-bridge** for TUTK to RTSP conversion (no separate container needed)
- Local streaming via RTSP (low latency)
- PTZ control for Pan cameras
- Motion detection events
- Snapshot capture

## How It Works

This plugin runs docker-wyze-bridge as an integrated subprocess:

1. **Login**: Authenticates with Wyze cloud API to get device list
2. **Bridge Start**: Spawns wyze-bridge Python process as a subprocess
3. **TUTK Connection**: wyze-bridge connects to cameras via P2P (TUTK protocol)
4. **RTSP Streaming**: Exposes camera streams as standard RTSP URLs

## Requirements

- **Python 3.8+**: Required for running the bundled wyze-bridge
- Wyze account with cameras

## Supported Devices

- Wyze Cam v1, v2, v3, v3 Pro
- Wyze Cam Pan, Pan v2, Pan v3, Pan Pro
- Wyze Cam Outdoor v1, v2
- Wyze Video Doorbell v1, v2, Pro
- Wyze Cam OG, OG Telephoto
- Wyze Cam Floodlight, Floodlight v2

## Installation

### Via SpatialNVR UI

1. Open SpatialNVR web interface
2. Navigate to Settings > Plugins
3. Click "Add Plugin"
4. Enter repository URL: `github.com/Spatial-NVR/wyze-plugin`
5. Click Install

### Manual Installation

1. Download the latest release from GitHub Releases

2. Extract to the plugins directory:
   ```bash
   mkdir -p /data/plugins/wyze
   tar -xzf wyze-plugin-*.tar.gz -C /data/plugins/wyze/
   ```

3. Ensure Python dependencies are installed:
   ```bash
   pip3 install -r /data/plugins/wyze/wyze-bridge/app/requirements.txt
   ```

4. Restart SpatialNVR

### Building from Source

```bash
# Clone the repository with submodules
git clone --recurse-submodules https://github.com/Spatial-NVR/wyze-plugin.git
cd wyze-plugin

# Build the plugin
./build.sh

# Copy to plugins directory (including wyze-bridge)
mkdir -p /data/plugins/wyze
cp wyze-plugin manifest.yaml /data/plugins/wyze/
cp -r wyze-bridge /data/plugins/wyze/
```

## Configuration

### Via Web UI

1. Navigate to Settings > Plugins > Wyze
2. Enter your Wyze account credentials
3. If you have 2FA enabled, provide your TOTP secret
4. Click "Connect" to authenticate
5. Select which cameras to add

### Via Config File

Add to your `config.yaml` under the `plugins` section:

```yaml
plugins:
  wyze:
    enabled: true
    config:
      email: your.email@example.com
      password: your_wyze_password
      # Optional: API key for improved authentication
      key_id: your_key_id
      api_key: your_api_key
      # Optional: TOTP secret for 2FA
      totp_key: your_totp_secret
      # Optional: Custom ports
      rtsp_port: 8554
      web_port: 5000
      # Optional: Filter specific cameras by MAC
      cameras:
        - mac: AABBCCDDEEFF
          name: Front Door
        - mac: 112233445566
          name: Backyard
```

## Authentication Options

### Basic (Email + Password)

The simplest method, but may require solving CAPTCHAs occasionally.

### API Keys (Recommended)

For improved authentication reliability:

1. Go to https://developer-api-console.wyze.com/
2. Create an API key
3. Add the `key_id` and `api_key` to your config

### 2FA/TOTP

If your Wyze account has 2FA enabled:

1. When setting up 2FA in Wyze, copy the TOTP secret (the text code, not QR)
2. Add it as `totp_key` in the config
3. The plugin will automatically generate codes for login

## Stream Access

Streams are provided via RTSP through the bundled wyze-bridge:

- **Main stream (HD)**: `rtsp://localhost:8554/{camera_name}`
- **Sub stream (SD)**: `rtsp://localhost:8554/{camera_name}_sub`
- **Snapshot**: `http://localhost:5000/img/{camera_name}.jpg`

Camera names are derived from Wyze nicknames with spaces and special characters replaced.

## API Reference

### Plugin RPC Methods

| Method | Description |
|--------|-------------|
| `initialize` | Initialize with Wyze credentials, starts bridge |
| `shutdown` | Stop bridge and cleanup |
| `health` | Get plugin health status (includes bridge status) |
| `discover_cameras` | List all Wyze cameras from account |
| `add_camera` | Add a camera by MAC address |
| `remove_camera` | Remove a camera |
| `list_cameras` | List configured cameras |
| `get_camera` | Get camera details |
| `ptz_control` | Send PTZ commands (Pan cameras only) |
| `get_snapshot` | Get snapshot URL |

### Health Status

The health endpoint includes bridge status:

```json
{
  "state": "healthy",
  "message": "2/2 cameras online",
  "details": {
    "cameras_online": 2,
    "cameras_total": 2,
    "authenticated": true,
    "bridge_running": true
  }
}
```

### PTZ Control (Pan Cameras)

```bash
curl -X POST http://localhost:12000/api/v1/plugins/wyze/cameras/{camera_id}/ptz \
  -H "Content-Type: application/json" \
  -d '{
    "command": "right",
    "speed": 5
  }'
```

Commands: `up`, `down`, `left`, `right`, `stop`

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                      SpatialNVR                              │
│  ┌─────────────────────────────────────────────────────────┐ │
│  │                    Wyze Plugin (Go)                      │ │
│  │  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐   │ │
│  │  │  Wyze API    │  │   Bridge     │  │   Camera     │   │ │
│  │  │  Client      │  │   Manager    │  │   Manager    │   │ │
│  │  └──────────────┘  └──────────────┘  └──────────────┘   │ │
│  └────────────────────────┬────────────────────────────────┘ │
│                           │ spawns                           │
│  ┌────────────────────────▼────────────────────────────────┐ │
│  │              wyze-bridge (Python subprocess)             │ │
│  │  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐   │ │
│  │  │  TUTK/P2P    │  │   MediaMTX   │  │   Web UI     │   │ │
│  │  │  Connector   │  │   (RTSP)     │  │   (API)      │   │ │
│  │  └──────────────┘  └──────────────┘  └──────────────┘   │ │
│  └─────────────────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────────────┘
                           │
                           │ RTSP streams
                           ▼
┌─────────────────────────────────────────────────────────────┐
│                      go2rtc / Web UI                         │
└─────────────────────────────────────────────────────────────┘
```

## Troubleshooting

### Bridge Not Starting

1. Verify Python 3.8+ is installed: `python3 --version`
2. Install requirements: `pip3 install flask paho-mqtt pydantic python-dotenv requests PyYAML xxtea`
3. Check plugin logs for Python errors

### Authentication Failed

1. Verify email and password are correct
2. If using 2FA, ensure TOTP secret is valid
3. Try using API keys for more reliable auth
4. Check if your account is locked (too many attempts)

### Camera Offline

1. Check camera power and WiFi connection
2. Verify camera works in Wyze app
3. Some cameras need firmware updates for TUTK

### Stream Not Playing

1. Wait for bridge to initialize (can take 30-60 seconds on first connect)
2. Check bridge is running in health status
3. Verify RTSP port (8554) is not blocked
4. Check wyze-bridge web UI at http://localhost:5000

### High Latency

1. TUTK P2P connection may route through relay servers
2. Ensure camera and NVR are on same network for best performance
3. Try sub-stream for lower latency

## Development

### Running Tests

```bash
go test -v ./...
```

### Building for Different Platforms

```bash
./build.sh
# Creates binaries for:
# - Linux AMD64
# - Linux ARM64
# - macOS AMD64
# - macOS ARM64
```

## License

MIT License - see [LICENSE](LICENSE) for details.

## Acknowledgments

- [docker-wyze-bridge](https://github.com/mrlt8/docker-wyze-bridge) - The Python bridge bundled with this plugin
- [wyze-sdk](https://github.com/shauntarves/wyze-sdk) - API documentation reference
