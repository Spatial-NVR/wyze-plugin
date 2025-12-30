#!/usr/bin/env python3
"""
Wyze Plugin for SpatialNVR
Uses wyzecam library directly for P2P camera connections (like Scrypted)
Communicates via JSON-RPC over stdin/stdout
Supports streaming mode for go2rtc exec source
"""

from __future__ import annotations

import argparse
import asyncio
import json
import os
import platform
import signal
import subprocess
import sys
import threading
import time
import traceback
import urllib.request
from ctypes import c_int
from typing import Any, Dict, List, Optional

# Add wyze-bridge wyzecam to path
PLUGIN_DIR = os.path.dirname(os.path.abspath(__file__))
WYZE_BRIDGE_DIR = os.path.join(PLUGIN_DIR, "wyze-bridge", "app")
if os.path.exists(WYZE_BRIDGE_DIR):
    sys.path.insert(0, WYZE_BRIDGE_DIR)

# Virtual environment Python path (used for go2rtc exec streams)
VENV_PYTHON = os.path.join(PLUGIN_DIR, "venv", "bin", "python3")

import wyzecam
from wyzecam.iotc import WyzeIOTC, WyzeIOTCSession

# TUTK SDK key (from docker-wyze-bridge)
SDK_KEY = "AQAAAIZ44fijz5pURQiNw4xpEfV9ZysFH8LYBPDxiONQlbLKaDeb7n26TSOPSGHftbRVo25k3uz5of06iGNB4pSfmvsCvm/tTlmML6HKS0vVxZnzEuK95TPGEGt+aE15m6fjtRXQKnUav59VSRHwRj9Z1Kjm1ClfkSPUF5NfUvsb3IAbai0WlzZE1yYCtks7NFRMbTXUMq3bFtNhEERD/7oc504b"

# Frame sizes
FRAME_SIZE_2K = 3
FRAME_SIZE_1080P = 1
FRAME_SIZE_360P = 2


def log(msg: str):
    """Log to stderr (stdout is for JSON-RPC or video data)"""
    print(f"[wyze] {msg}", file=sys.stderr, flush=True)


def format_exception(e: Exception) -> str:
    return "\n".join(traceback.format_exception(e))


def get_tutk_library() -> Optional[str]:
    """Get or download the TUTK library for the current platform"""
    machine = platform.machine()
    if machine == "x86_64":
        suffix = "amd64"
    elif machine in ("aarch64", "arm64"):
        suffix = "arm64"
    else:
        log(f"Unsupported architecture: {machine}")
        return None

    lib_dir = os.path.join(PLUGIN_DIR, "lib")
    os.makedirs(lib_dir, exist_ok=True)

    lib_path = os.path.join(lib_dir, f"lib.{suffix}")
    if os.path.exists(lib_path):
        return lib_path

    # Download from docker-wyze-bridge (lib files are in app/lib/ subdirectory)
    url = f"https://github.com/mrlt8/docker-wyze-bridge/raw/main/app/lib/lib.{suffix}"
    log(f"Downloading TUTK library from {url}...")

    try:
        tmp_path = lib_path + ".tmp"
        urllib.request.urlretrieve(url, tmp_path)
        os.rename(tmp_path, lib_path)
        os.chmod(lib_path, 0o755)
        log(f"TUTK library downloaded: {lib_path}")
        return lib_path
    except Exception as e:
        log(f"Failed to download TUTK library: {e}")
        return None


def load_config() -> Dict[str, Any]:
    """Load plugin configuration from file"""
    config_path = os.path.join(PLUGIN_DIR, "config.json")
    if os.path.exists(config_path):
        with open(config_path) as f:
            return json.load(f)
    return {}


def save_config(config: Dict[str, Any]):
    """Save plugin configuration to file"""
    config_path = os.path.join(PLUGIN_DIR, "config.json")
    with open(config_path, "w") as f:
        json.dump(config, f, indent=2)


def load_auth_cache() -> Optional[Dict[str, Any]]:
    """Load cached authentication data"""
    cache_path = os.path.join(PLUGIN_DIR, "auth_cache.json")
    if os.path.exists(cache_path):
        try:
            with open(cache_path) as f:
                cache = json.load(f)
                # Check if cache is still valid (tokens expire, but we cache for 1 hour)
                cached_time = cache.get("cached_at", 0)
                if time.time() - cached_time < 3600:  # 1 hour cache
                    return cache
        except Exception as e:
            log(f"Failed to load auth cache: {e}")
    return None


def save_auth_cache(auth_info: Any, account: Any, cameras: Dict[str, Any]):
    """Save authentication data to cache"""
    cache_path = os.path.join(PLUGIN_DIR, "auth_cache.json")
    try:
        # Serialize auth info and cameras
        cache = {
            "cached_at": time.time(),
            "auth_info": auth_info.model_dump() if hasattr(auth_info, 'model_dump') else auth_info.__dict__,
            "account": account.model_dump() if hasattr(account, 'model_dump') else account.__dict__,
            "cameras": {mac: (cam.model_dump() if hasattr(cam, 'model_dump') else cam.__dict__)
                       for mac, cam in cameras.items()}
        }
        with open(cache_path, "w") as f:
            json.dump(cache, f, indent=2)
        log("Auth cache saved")
    except Exception as e:
        log(f"Failed to save auth cache: {e}")


class WyzeAuth:
    """Manages Wyze authentication"""

    def __init__(self, config: Dict[str, Any]):
        self.config = config
        self.auth_info: Optional[wyzecam.WyzeCredential] = None
        self.account: Optional[wyzecam.WyzeAccount] = None
        self.cameras: Dict[str, wyzecam.WyzeCamera] = {}

    def login(self, use_cache: bool = True):
        """Login to Wyze and get camera list (with caching)"""
        # Try to use cached auth first
        if use_cache:
            cache = load_auth_cache()
            if cache:
                try:
                    log("Using cached authentication")
                    self.auth_info = wyzecam.WyzeCredential.model_validate(cache["auth_info"])
                    self.account = wyzecam.WyzeAccount.model_validate(cache["account"])
                    for mac, cam_data in cache["cameras"].items():
                        self.cameras[mac] = wyzecam.WyzeCamera.model_validate(cam_data)
                    log(f"Loaded {len(self.cameras)} cameras from cache")
                    return self
                except Exception as e:
                    log(f"Cache load failed, will re-authenticate: {e}")

        email = self.config.get("email")
        password = self.config.get("password")
        key_id = self.config.get("key_id")
        api_key = self.config.get("api_key")

        if not email or not password:
            raise ValueError("email and password are required")

        log(f"Logging into Wyze as {email}...")
        self.auth_info = wyzecam.login(
            email, password,
            api_key=api_key,
            key_id=key_id
        )
        self.account = wyzecam.get_user_info(self.auth_info)
        log(f"Logged in successfully as {self.account.nickname}")

        # Get cameras
        camera_list = wyzecam.get_camera_list(self.auth_info)
        for camera in camera_list:
            self.cameras[camera.mac] = camera
            log(f"Found camera: {camera.nickname} ({camera.mac}) - {camera.product_model}")

        # Save to cache
        save_auth_cache(self.auth_info, self.account, self.cameras)

        return self

    def get_camera(self, mac: str) -> Optional[wyzecam.WyzeCamera]:
        """Get a camera by MAC address"""
        return self.cameras.get(mac)


def stream_camera(mac: str):
    """Stream a camera to stdout using FFmpeg

    This is called by go2rtc via exec: source.
    Connects to camera via TUTK P2P and pipes video/audio through FFmpeg.
    """
    log(f"Starting stream for camera {mac}")

    # Load config and authenticate
    config = load_config()
    if not config:
        log("No configuration found. Initialize the plugin first.")
        sys.exit(1)

    auth = WyzeAuth(config)
    try:
        auth.login()
    except Exception as e:
        log(f"Authentication failed: {e}")
        sys.exit(1)

    camera = auth.get_camera(mac)
    if not camera:
        log(f"Camera not found: {mac}")
        log(f"Available cameras: {list(auth.cameras.keys())}")
        sys.exit(1)

    log(f"Connecting to {camera.nickname}...")
    log(f"Camera p2p_id={getattr(camera, 'p2p_id', 'N/A')}, model={camera.product_model}")
    log(f"Camera dtls={getattr(camera, 'dtls', 'N/A')}, parent_dtls={getattr(camera, 'parent_dtls', 'N/A')}")
    log(f"Camera enr={getattr(camera, 'enr', 'N/A')[:8] if hasattr(camera, 'enr') and camera.enr else 'N/A'}...")

    # Check required camera fields
    if not getattr(camera, 'p2p_id', None):
        log(f"ERROR: Camera {camera.nickname} missing p2p_id - cannot connect via P2P")
        sys.exit(1)
    if not getattr(camera, 'enr', None):
        log(f"ERROR: Camera {camera.nickname} missing enr - cannot authenticate")
        sys.exit(1)

    # Get TUTK library
    tutk_lib = get_tutk_library()
    if not tutk_lib:
        log("Failed to get TUTK library")
        sys.exit(1)

    # Set environment
    os.environ["TUTK_PROJECT_ROOT"] = os.path.dirname(tutk_lib)

    # Initialize TUTK
    iotc = WyzeIOTC(
        tutk_platform_lib=tutk_lib,
        sdk_key=SDK_KEY,
        max_num_av_channels=1,
    )
    iotc.initialize()

    # Determine quality settings
    frame_size = FRAME_SIZE_1080P
    bitrate = 120
    if camera.product_model in ("WYZECP1", "HL_CAM3P", "WYZE_CAKP2JFUS"):
        # Pan cameras and newer models support 2K
        if hasattr(camera, 'is_2k') and camera.is_2k:
            frame_size = FRAME_SIZE_2K
            bitrate = 180

    log(f"Using frame_size={frame_size}, bitrate={bitrate}")

    # Start FFmpeg process to convert raw H264 to RTSP
    # FFmpeg reads from stdin (pipe) and outputs to stdout
    ffmpeg_cmd = [
        "ffmpeg",
        "-hide_banner",
        "-loglevel", "error",
        "-f", "h264",
        "-i", "pipe:0",
        "-c:v", "copy",
        "-f", "rtsp",
        "-rtsp_transport", "tcp",
        "pipe:1"
    ]

    # Actually, go2rtc exec expects raw video output that it can handle
    # Let's output raw H264 directly - go2rtc can handle h264 raw streams
    # via exec:ffmpeg ... -f h264 pipe: format

    try:
        log("Starting TUTK P2P connection (timeout=30s)...")
        with WyzeIOTCSession(
            iotc.tutk_platform_lib,
            auth.account,
            camera,
            frame_size=frame_size,
            bitrate=bitrate,
            connect_timeout=30,  # Increase timeout from default 20s
        ) as session:
            log(f"Connected to {camera.nickname}, starting stream...")

            # Stream video frames to stdout
            # recv_video_data yields raw H264 NAL units
            for frame in session.recv_video_data():
                if frame:
                    # Output raw H264 data to stdout
                    sys.stdout.buffer.write(frame)
                    sys.stdout.buffer.flush()

    except KeyboardInterrupt:
        log("Stream interrupted")
    except Exception as e:
        # Log error details on separate lines to avoid truncation
        log(f"Stream error type: {type(e).__name__}")
        log(f"Stream error message: {str(e)}")
        # Log traceback lines individually
        for line in traceback.format_exception(e):
            for subline in line.strip().split('\n'):
                if subline.strip():
                    log(f"  {subline}")
    finally:
        try:
            iotc.deinitialize()
        except:
            pass

    log("Stream ended")


class WyzePlugin:
    """Main plugin class for JSON-RPC communication"""

    def __init__(self):
        self.config: Dict[str, Any] = {}
        self.auth: Optional[WyzeAuth] = None
        self.tutk_lib: Optional[str] = None
        self.running = True

    def initialize(self, config: Dict[str, Any]) -> Dict[str, Any]:
        """Initialize the plugin with configuration"""
        self.config = config

        # Save config for streaming subprocess
        save_config(config)

        # Get TUTK library
        self.tutk_lib = get_tutk_library()
        if not self.tutk_lib:
            raise RuntimeError("Failed to download TUTK library")

        # Authenticate and get cameras
        self.auth = WyzeAuth(config)
        self.auth.login()

        return {"status": "ok", "cameras": len(self.auth.cameras)}

    def shutdown(self) -> Dict[str, Any]:
        """Shutdown the plugin"""
        log("Shutting down...")
        self.running = False
        return {"status": "ok"}

    def health(self) -> Dict[str, Any]:
        """Return health status"""
        if not self.auth or not self.auth.auth_info:
            return {
                "state": "unhealthy",
                "message": "Not authenticated to Wyze",
                "last_check": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
                "details": {"authenticated": False}
            }

        return {
            "state": "healthy",
            "message": f"{len(self.auth.cameras)} cameras available",
            "last_check": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
            "details": {
                "cameras_total": len(self.auth.cameras),
                "authenticated": True,
            }
        }

    def discover_cameras(self) -> List[Dict[str, Any]]:
        """Return list of discovered cameras"""
        if not self.auth:
            return []

        result = []
        for camera in self.auth.cameras.values():
            result.append({
                "id": camera.mac,
                "name": camera.nickname,
                "model": camera.product_model,
                "manufacturer": "Wyze",
                "capabilities": self._get_capabilities(camera),
                "firmware_version": getattr(camera, 'firmware_ver', ''),
                "serial": camera.mac,
            })
        return result

    def _get_stream_url(self, camera: wyzecam.WyzeCamera) -> str:
        """Get the stream URL for a camera using exec source"""
        # Use exec: source so go2rtc runs FFmpeg to read our raw H264 output
        plugin_path = os.path.abspath(__file__)
        # go2rtc exec format: ffmpeg reads from our script's stdout (use venv python)
        return f"exec:ffmpeg -hide_banner -loglevel error -f h264 -i $({VENV_PYTHON} {plugin_path} stream {camera.mac}) -c:v copy -f rtsp {{output}}"

    def _get_stream_url_simple(self, camera: wyzecam.WyzeCamera) -> str:
        """Get simple exec stream URL"""
        plugin_path = os.path.abspath(__file__)
        # Direct exec - use venv python so dependencies are available
        return f"exec:{VENV_PYTHON} {plugin_path} stream {camera.mac}#video=h264"

    def list_cameras(self) -> List[Dict[str, Any]]:
        """Return list of configured cameras with stream URLs"""
        if not self.auth:
            return []

        result = []
        for mac, camera in self.auth.cameras.items():
            # Use exec source for go2rtc with venv python
            plugin_path = os.path.abspath(__file__)
            stream_url = f"exec:{VENV_PYTHON} {plugin_path} stream {mac}#video=h264"

            result.append({
                "id": camera.mac,
                "plugin_id": "wyze",
                "name": camera.nickname,
                "model": camera.product_model,
                "host": getattr(camera, 'ip', '') or "",
                "main_stream": stream_url,
                "sub_stream": "",
                "snapshot_url": "",
                "capabilities": self._get_capabilities(camera),
                "online": True,
                "last_seen": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
            })
        return result

    def get_camera(self, camera_id: str) -> Optional[Dict[str, Any]]:
        """Get a specific camera"""
        cameras = self.list_cameras()
        for cam in cameras:
            if cam["id"] == camera_id:
                return cam
        return None

    def add_camera(self, mac: str, name: Optional[str] = None) -> Optional[Dict[str, Any]]:
        """Add a camera by MAC address"""
        if not self.auth:
            return None

        camera = self.auth.get_camera(mac)
        if not camera:
            return None

        plugin_path = os.path.abspath(__file__)
        stream_url = f"exec:{VENV_PYTHON} {plugin_path} stream {mac}#video=h264"

        return {
            "id": camera.mac,
            "plugin_id": "wyze",
            "name": name or camera.nickname,
            "model": camera.product_model,
            "manufacturer": "Wyze",
            "host": getattr(camera, 'ip', '') or "",
            "main_stream": stream_url,
            "sub_stream": "",
            "snapshot_url": "",
            "capabilities": self._get_capabilities(camera),
            "online": True,
            "last_seen": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        }

    def _get_capabilities(self, camera: wyzecam.WyzeCamera) -> List[str]:
        """Get camera capabilities"""
        caps = ["video"]
        # Check for audio support
        if hasattr(camera, 'audio') and camera.audio:
            caps.append("audio")
        # Check for PTZ
        if camera.product_model in ("WYZECP1", "HL_PAN2", "HL_PAN3"):
            caps.append("ptz")
        return caps

    def handle_request(self, request: Dict[str, Any]) -> Dict[str, Any]:
        """Handle a JSON-RPC request"""
        method = request.get("method", "")
        params = request.get("params", {})
        req_id = request.get("id")

        response = {
            "jsonrpc": "2.0",
            "id": req_id,
        }

        try:
            if method == "initialize":
                response["result"] = self.initialize(params)
            elif method == "shutdown":
                response["result"] = self.shutdown()
            elif method == "health":
                response["result"] = self.health()
            elif method == "discover_cameras":
                response["result"] = self.discover_cameras()
            elif method == "list_cameras":
                response["result"] = self.list_cameras()
            elif method == "get_camera":
                camera_id = params.get("camera_id")
                result = self.get_camera(camera_id)
                if result:
                    response["result"] = result
                else:
                    response["error"] = {"code": -32603, "message": "Camera not found"}
            elif method == "add_camera":
                mac = params.get("mac")
                name = params.get("name")
                result = self.add_camera(mac, name)
                if result:
                    response["result"] = result
                else:
                    response["error"] = {"code": -32603, "message": f"Camera not found: {mac}"}
            else:
                response["error"] = {"code": -32601, "message": f"Method not found: {method}"}
        except Exception as e:
            log(f"Error handling {method}: {format_exception(e)}")
            response["error"] = {"code": -32603, "message": str(e)}

        return response


def run_jsonrpc():
    """Run the plugin in JSON-RPC mode"""
    log("Wyze plugin starting in JSON-RPC mode...")

    plugin = WyzePlugin()

    def signal_handler(signum, frame):
        log("Received signal, shutting down...")
        plugin.shutdown()
        sys.exit(0)

    signal.signal(signal.SIGINT, signal_handler)
    signal.signal(signal.SIGTERM, signal_handler)

    # Read JSON-RPC requests from stdin
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue

        try:
            request = json.loads(line)
            response = plugin.handle_request(request)
            print(json.dumps(response), flush=True)
        except json.JSONDecodeError as e:
            log(f"Invalid JSON: {e}")
            print(json.dumps({
                "jsonrpc": "2.0",
                "id": None,
                "error": {"code": -32700, "message": "Parse error"}
            }), flush=True)


def main():
    """Main entry point"""
    parser = argparse.ArgumentParser(description="Wyze Plugin for SpatialNVR")
    parser.add_argument("command", nargs="?", default="jsonrpc",
                       help="Command: jsonrpc (default) or stream")
    parser.add_argument("camera_mac", nargs="?",
                       help="Camera MAC address (for stream command)")

    args = parser.parse_args()

    if args.command == "stream":
        if not args.camera_mac:
            log("Camera MAC address required for stream command")
            sys.exit(1)
        stream_camera(args.camera_mac)
    else:
        run_jsonrpc()


if __name__ == "__main__":
    main()
