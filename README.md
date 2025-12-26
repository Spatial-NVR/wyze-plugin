# Wyze Plugin for SpatialNVR

A plugin for integrating Wyze cameras with SpatialNVR using the TUTK library for local streaming.

## Features

- Wyze account authentication (including 2FA/TOTP)
- Automatic camera discovery from Wyze account
- Local streaming via TUTK library (no cloud dependency for video)
- PTZ control for Pan cameras
- Motion detection events
- Low latency streaming

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

3. Restart SpatialNVR

4. The TUTK library will be downloaded automatically on first run

### Building from Source

```bash
# Clone the repository
git clone https://github.com/Spatial-NVR/wyze-plugin.git
cd wyze-plugin

# Build the plugin
go build -o wyze-plugin .

# Copy to plugins directory
mkdir -p /data/plugins/wyze
cp wyze-plugin manifest.yaml /data/plugins/wyze/
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

## API Reference

### Plugin RPC Methods

| Method | Description |
|--------|-------------|
| `initialize` | Initialize with Wyze credentials |
| `shutdown` | Graceful shutdown |
| `health` | Get plugin health status |
| `discover_cameras` | List all Wyze cameras from account |
| `add_camera` | Add a camera by MAC address |
| `remove_camera` | Remove a camera |
| `list_cameras` | List configured cameras |
| `get_camera` | Get camera details |
| `ptz_control` | Send PTZ commands (Pan cameras only) |
| `get_snapshot` | Capture from stream |

### Discovering Cameras

```bash
curl http://localhost:12000/api/v1/plugins/wyze/discover
```

Response:
```json
{
  "cameras": [
    {
      "mac": "AABBCCDDEEFF",
      "nickname": "Front Door",
      "model": "WYZE_CAKP2JFUS",
      "model_name": "Wyze Cam v3",
      "is_online": true,
      "has_ptz": false
    }
  ]
}
```

### Adding a Camera

```bash
curl -X POST http://localhost:12000/api/v1/plugins/wyze/cameras \
  -H "Content-Type: application/json" \
  -d '{
    "mac": "AABBCCDDEEFF",
    "name": "Front Door"
  }'
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

## Stream Access

Streams are provided via RTSP through the TUTK connection:

- **Main stream**: `rtsp://localhost:8554/{camera_name}`
- **Sub stream**: `rtsp://localhost:8554/{camera_name}_sub`

Camera names are derived from Wyze nicknames with spaces replaced by underscores.

## TUTK Library

The plugin uses the TUTK (ThroughTek Kalay) library for direct P2P connections to cameras. This enables local streaming without routing through Wyze cloud servers.

The library is automatically downloaded on first run:
- Linux amd64: `tutk-ioctl-linux-amd64`
- Linux arm64: `tutk-ioctl-linux-arm64`
- macOS amd64: `tutk-ioctl-darwin-amd64`
- macOS arm64: `tutk-ioctl-darwin-arm64`

## Comparison with docker-wyze-bridge

This plugin is inspired by [docker-wyze-bridge](https://github.com/mrlt8/docker-wyze-bridge) but runs as a native SpatialNVR plugin.

| Feature | wyze-plugin | docker-wyze-bridge |
|---------|-------------|-------------------|
| Docker required | No | Yes |
| Resource usage | Lower | Higher |
| NVR integration | Native | External |
| Updates | Plugin system | Docker pull |
| Configuration | SpatialNVR config | Separate env vars |

## Troubleshooting

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

1. Wait for TUTK connection to establish (can take 10-30 seconds)
2. Check if camera supports the requested stream quality
3. Verify RTSP port (8554) is not blocked

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
# Linux AMD64
GOOS=linux GOARCH=amd64 go build -o wyze-plugin-linux-amd64 .

# Linux ARM64 (Raspberry Pi 4)
GOOS=linux GOARCH=arm64 go build -o wyze-plugin-linux-arm64 .

# macOS
GOOS=darwin GOARCH=arm64 go build -o wyze-plugin-darwin-arm64 .
```

## License

MIT License - see [LICENSE](LICENSE) for details.

## Contributing

1. Fork the repository
2. Create a feature branch
3. Make your changes
4. Run tests
5. Submit a pull request

See the main [SpatialNVR Contributing Guide](https://github.com/Spatial-NVR/SpatialNVR/blob/main/docs/CONTRIBUTING.md) for style guidelines.

## Acknowledgments

- [docker-wyze-bridge](https://github.com/mrlt8/docker-wyze-bridge) for TUTK integration inspiration
- [wyze-sdk](https://github.com/shauntarves/wyze-sdk) for API documentation
