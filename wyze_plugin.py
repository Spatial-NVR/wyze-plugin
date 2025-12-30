#!/usr/bin/env python3
"""
Wyze Plugin for SpatialNVR
Uses wyzecam library directly for P2P camera connections (like Scrypted)
Communicates via JSON-RPC over stdin/stdout
"""

from __future__ import annotations

import asyncio
import base64
import concurrent.futures
import json
import os
import platform
import signal
import struct
import sys
import threading
import time
import traceback
import socket
import urllib.request
from ctypes import c_int
from typing import Any, Dict, List, Optional

# Add wyzecam to path (downloaded to plugin directory)
PLUGIN_DIR = os.path.dirname(os.path.abspath(__file__))
WYZECAM_DIR = os.path.join(PLUGIN_DIR, "wyzecam")
if os.path.exists(WYZECAM_DIR):
    sys.path.insert(0, WYZECAM_DIR)

import wyzecam
import wyzecam.api_models
from wyzecam import tutk_protocol
from wyzecam.api import RateLimitError, post_device

# TUTK SDK key (from Scrypted)
SDK_KEY = "AQAAAIZ44fijz5pURQiNw4xpEfV9ZysFH8LYBPDxiONQlbLKaDeb7n26TSOPSGHftbRVo25k3uz5of06iGNB4pSfmvsCvm/tTlmML6HKS0vVxZnzEuK95TPGEGt+aE15m6fjtRXQKnUav59VSRHwRj9Z1Kjm1ClfkSPUF5NfUvsb3IAbai0WlzZE1yYCtks7NFRMbTXUMq3bFtNhEERD/7oc504b"

# Frame sizes
FRAME_SIZE_2K = 3
FRAME_SIZE_1080P = 1
FRAME_SIZE_360P = 2

# Default ports
DEFAULT_RTSP_PORT = 8564


def log(msg: str):
    """Log to stderr (stdout is for JSON-RPC)"""
    print(msg, file=sys.stderr, flush=True)


def format_exception(e: Exception) -> str:
    return "\n".join(traceback.format_exception(e))


class WyzePlugin:
    """Main plugin class"""

    def __init__(self):
        self.config: Dict[str, Any] = {}
        self.auth_info: Optional[wyzecam.WyzeCredential] = None
        self.account: Optional[wyzecam.WyzeAccount] = None
        self.cameras: Dict[str, wyzecam.WyzeCamera] = {}
        self.camera_streams: Dict[str, CameraStream] = {}
        self.tutk_platform_lib: Optional[str] = None
        self.wyze_iotc: Optional[wyzecam.WyzeIOTC] = None
        self.rtsp_port = DEFAULT_RTSP_PORT
        self.running = True

    def initialize(self, config: Dict[str, Any]) -> Dict[str, Any]:
        """Initialize the plugin with configuration"""
        self.config = config

        email = config.get("email")
        password = config.get("password")
        key_id = config.get("key_id")
        api_key = config.get("api_key")

        if not email or not password:
            raise ValueError("email and password are required")

        self.rtsp_port = config.get("rtsp_port", DEFAULT_RTSP_PORT)

        # Download TUTK library
        self.tutk_platform_lib = self._download_tutk_library()
        if not self.tutk_platform_lib:
            raise RuntimeError("Failed to download TUTK library")

        # Set TUTK environment
        os.environ["TUTK_PROJECT_ROOT"] = os.path.dirname(self.tutk_platform_lib)

        # Initialize TUTK
        self.wyze_iotc = wyzecam.WyzeIOTC(
            tutk_platform_lib=self.tutk_platform_lib,
            sdk_key=SDK_KEY,
            max_num_av_channels=32,
        )
        self.wyze_iotc.initialize()

        # Login to Wyze
        log(f"Logging into Wyze as {email}...")
        self.auth_info = wyzecam.login(
            email, password,
            api_key=api_key,
            key_id=key_id
        )
        self.account = wyzecam.get_user_info(self.auth_info)
        log(f"Logged in successfully")

        # Get cameras
        camera_list = wyzecam.get_camera_list(self.auth_info)
        for camera in camera_list:
            self.cameras[camera.mac] = camera
            log(f"Found camera: {camera.nickname} ({camera.mac})")

            # Start stream server for each camera (using shared TUTK instance)
            stream = CameraStream(
                camera=camera,
                account=self.account,
                wyze_iotc=self.wyze_iotc,
                port=self.rtsp_port + len(self.camera_streams)
            )
            self.camera_streams[camera.mac] = stream
            stream.start()

        return {"status": "ok", "cameras": len(self.cameras)}

    def _download_tutk_library(self) -> Optional[str]:
        """Download the TUTK library for the current platform"""
        machine = platform.machine()
        if machine == "x86_64":
            suffix = "amd64"
        elif machine == "aarch64":
            suffix = "arm64"
        else:
            log(f"Unsupported architecture: {machine}")
            return None

        lib_dir = os.path.join(PLUGIN_DIR, "lib")
        os.makedirs(lib_dir, exist_ok=True)

        lib_path = os.path.join(lib_dir, f"lib.{suffix}")
        if os.path.exists(lib_path):
            log(f"TUTK library already exists: {lib_path}")
            return lib_path

        # Download from koush's fork (same as Scrypted)
        url = f"https://github.com/koush/docker-wyze-bridge/raw/main/app/lib.{suffix}"
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

    def shutdown(self) -> Dict[str, Any]:
        """Shutdown the plugin"""
        log("Shutting down...")
        self.running = False

        for stream in self.camera_streams.values():
            stream.stop()

        if self.wyze_iotc:
            try:
                self.wyze_iotc.deinitialize()
            except:
                pass

        return {"status": "ok"}

    def health(self) -> Dict[str, Any]:
        """Return health status"""
        # Count cameras with active stream servers (ready to accept connections)
        servers_running = sum(1 for s in self.camera_streams.values() if s.running)
        active_connections = sum(1 for s in self.camera_streams.values() if s.is_connected)
        total = len(self.cameras)

        # Plugin is healthy if authenticated and all stream servers are running
        # Cameras don't need active connections to be "healthy" - they connect on-demand
        if not self.auth_info:
            state = "unhealthy"
            message = "Not authenticated to Wyze"
        elif servers_running < total:
            state = "degraded"
            message = f"{servers_running}/{total} stream servers running"
        else:
            state = "healthy"
            message = f"{total} cameras ready, {active_connections} streaming"

        return {
            "state": state,
            "message": message,
            "last_check": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
            "details": {
                "cameras_total": total,
                "servers_running": servers_running,
                "active_streams": active_connections,
                "authenticated": self.auth_info is not None,
            }
        }

    def discover_cameras(self) -> List[Dict[str, Any]]:
        """Return list of discovered cameras"""
        result = []
        for camera in self.cameras.values():
            result.append({
                "id": camera.mac,
                "name": camera.nickname,
                "model": camera.model_name,
                "manufacturer": "Wyze",
                "capabilities": self._get_capabilities(camera),
                "firmware_version": camera.firmware_ver,
                "serial": camera.mac,
            })
        return result

    def list_cameras(self) -> List[Dict[str, Any]]:
        """Return list of configured cameras with stream URLs"""
        result = []
        for mac, camera in self.cameras.items():
            stream = self.camera_streams.get(mac)
            rtsp_url = f"rtsp://127.0.0.1:{stream.port}/{camera.mac}" if stream else ""

            result.append({
                "id": camera.mac,
                "plugin_id": "wyze",
                "name": camera.nickname,
                "model": camera.model_name,
                "host": camera.ip or "",
                "main_stream": rtsp_url,
                "sub_stream": "",
                "snapshot_url": "",
                "capabilities": self._get_capabilities(camera),
                "online": stream.is_connected if stream else False,
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
        """Add a camera by MAC address - returns camera info with stream URL.

        Since all cameras are auto-started during initialize(), this just
        returns the camera data for the NVR to register it.
        """
        camera = self.cameras.get(mac)
        if not camera:
            return None

        stream = self.camera_streams.get(mac)
        rtsp_url = f"rtsp://127.0.0.1:{stream.port}/{camera.mac}" if stream else ""

        return {
            "id": camera.mac,
            "plugin_id": "wyze",
            "name": name or camera.nickname,
            "model": camera.model_name,
            "manufacturer": "Wyze",
            "host": camera.ip or "",
            "main_stream": rtsp_url,
            "sub_stream": "",
            "snapshot_url": "",
            "capabilities": self._get_capabilities(camera),
            "online": stream.is_connected if stream else False,
            "last_seen": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        }

    def _get_capabilities(self, camera: wyzecam.WyzeCamera) -> List[str]:
        """Get camera capabilities"""
        caps = ["video", "audio"]
        if camera.is_pan_cam:
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


class CameraStream:
    """Handles streaming from a single Wyze camera"""

    def __init__(
        self,
        camera: wyzecam.WyzeCamera,
        account: wyzecam.WyzeAccount,
        wyze_iotc: wyzecam.WyzeIOTC,
        port: int
    ):
        self.camera = camera
        self.account = account
        self.wyze_iotc = wyze_iotc  # Shared, already-initialized TUTK instance
        self.port = port
        self.is_connected = False
        self.running = False
        self.server: Optional[asyncio.AbstractServer] = None
        self.loop: Optional[asyncio.AbstractEventLoop] = None
        self.thread: Optional[threading.Thread] = None

    def start(self):
        """Start the stream server in a background thread"""
        self.running = True
        self.thread = threading.Thread(target=self._run_server, daemon=True)
        self.thread.start()

    def stop(self):
        """Stop the stream server"""
        self.running = False
        if self.loop:
            self.loop.call_soon_threadsafe(self._stop_server)

    def _stop_server(self):
        if self.server:
            self.server.close()

    def _run_server(self):
        """Run the async server in this thread"""
        self.loop = asyncio.new_event_loop()
        asyncio.set_event_loop(self.loop)

        try:
            self.loop.run_until_complete(self._serve())
        except Exception as e:
            log(f"Stream server error for {self.camera.nickname}: {e}")
        finally:
            self.loop.close()

    async def _serve(self):
        """Start TCP server for RFC4571 streams"""
        self.server = await asyncio.start_server(
            self._handle_client,
            "0.0.0.0",
            self.port
        )
        log(f"Stream server for {self.camera.nickname} listening on port {self.port}")

        async with self.server:
            await self.server.serve_forever()

    async def _handle_client(
        self,
        reader: asyncio.StreamReader,
        writer: asyncio.StreamWriter
    ):
        """Handle a client connection - stream video data"""
        log(f"Client connected to {self.camera.nickname}")
        self.is_connected = True

        try:
            await self._stream_to_client(writer)
        except Exception as e:
            log(f"Stream error: {e}")
        finally:
            self.is_connected = False
            writer.close()
            await writer.wait_closed()
            log(f"Client disconnected from {self.camera.nickname}")

    async def _stream_to_client(self, writer: asyncio.StreamWriter):
        """Stream video/audio to client using TUTK P2P"""
        # Use the shared, already-initialized TUTK instance
        frame_size = FRAME_SIZE_2K if self.camera.is_2k else FRAME_SIZE_1080P
        bitrate = 240 if self.camera.is_2k else 160

        queue: asyncio.Queue = asyncio.Queue()
        closed = False

        def receive_frames():
            nonlocal closed
            try:
                with wyzecam.WyzeIOTCSession(
                    self.wyze_iotc.tutk_platform_lib,
                    self.account,
                    self.camera,
                    frame_size=frame_size,
                    bitrate=bitrate,
                    enable_audio=True,
                    stream_state=c_int(2),
                ) as sess:
                    for frame in sess.recv_bridge_data():
                        if closed or not self.running:
                            break
                        asyncio.run_coroutine_threadsafe(
                            queue.put(frame),
                            self.loop
                        )
            except Exception as e:
                log(f"TUTK session error: {e}")
            finally:
                closed = True
                asyncio.run_coroutine_threadsafe(queue.put(None), self.loop)

        # Start frame receiver in background thread
        recv_thread = threading.Thread(target=receive_frames, daemon=True)
        recv_thread.start()

        try:
            while self.running and not closed:
                frame = await queue.get()
                if frame is None:
                    break

                # Write RFC4571 framed data (length prefix + data)
                length = struct.pack(">H", len(frame))
                writer.write(length + frame)
                await writer.drain()
        finally:
            closed = True
            # Don't deinitialize - it's a global resource


def main():
    """Main entry point"""
    log("Wyze plugin starting...")

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


if __name__ == "__main__":
    main()
