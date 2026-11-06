"""Hand-written tests for the GPU.ai Open Source SDK configuration and a
round trip against a local stub server. Not generated; the generator leaves
this file alone."""

import json
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer
from unittest import mock

import gpuai_sdk


class _Handler(BaseHTTPRequestHandler):
    seen = {}

    def do_GET(self):  # noqa: N802
        type(self).seen = {
            "path": self.path,
            "auth": self.headers.get("Authorization"),
            "ua": self.headers.get("User-Agent"),
        }
        body = json.dumps({
            "data": [{
                "id": "key_1",
                "name": "laptop",
                "fingerprint": "SHA256:abc",
                "created_at": "2026-01-01T00:00:00Z",
            }],
            "next_cursor": None,
        }).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


class TestOSSClient(unittest.TestCase):
    def test_default_host_is_local_server(self):
        with mock.patch.dict("os.environ", {}, clear=True):
            self.assertEqual(gpuai_sdk.Configuration().host, "http://localhost:8080/v1")

    def test_env_overrides_default_host(self):
        with mock.patch.dict("os.environ", {"GPUAI_API_BASE": "http://gpu.example.test:9000/v1"}):
            self.assertEqual(gpuai_sdk.Configuration().host, "http://gpu.example.test:9000/v1")

    def test_explicit_host_wins_over_env(self):
        with mock.patch.dict("os.environ", {"GPUAI_API_BASE": "http://gpu.example.test:9000/v1"}):
            cfg = gpuai_sdk.Configuration(host="http://other.example.test/v1")
            self.assertEqual(cfg.host, "http://other.example.test/v1")

    def test_list_ssh_keys_round_trip(self):
        server = HTTPServer(("127.0.0.1", 0), _Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            cfg = gpuai_sdk.Configuration(
                host=f"http://127.0.0.1:{server.server_port}/v1",
                access_token="gpuai_test_key",
            )
            with gpuai_sdk.ApiClient(cfg) as client:
                page = gpuai_sdk.SshKeysApi(client).list_ssh_keys()
            self.assertEqual(_Handler.seen["path"], "/v1/ssh-keys")
            self.assertEqual(_Handler.seen["auth"], "Bearer gpuai_test_key")
            self.assertTrue(_Handler.seen["ua"].startswith("gpuai-sdk-python/"))
            self.assertEqual(page.data[0].name, "laptop")
            self.assertIsNone(page.next_cursor)
        finally:
            server.shutdown()
            server.server_close()


if __name__ == "__main__":
    unittest.main()
