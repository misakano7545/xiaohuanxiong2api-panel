"""本地调试用的 Qoder 上游桩：只为验证已部署二进制的「签名→组包→SSE 转 OpenAI」链路。
用法：python3 qoder_stub.py 7899   然后让网关的 gateway_host 指向它。
不在生产路径上，不要挂到隧道里。"""
import json, sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

CATALOG = {"chat": [
    {"key": "qmodel_38max", "display_name": "Qwen3.8-Max", "is_reasoning": True, "is_vl": True,
     "enable": True, "max_input_tokens": 200000, "price_factor": 0.5,
     "thinking_config": {"enabled": {"efforts": {"low": {}, "medium": {}, "xhigh": {}}}}},
    {"key": "qfmodel", "display_name": "Qwen3.8-Flash", "is_reasoning": True, "is_vl": True,
     "enable": True, "max_input_tokens": 200000, "price_factor": 0.1},
]}
QUOTA = {"userQuota": {"remaining": 120, "total": 200, "used": 80}, "expiresAt": 1900000000000}
CHUNKS = [
    {"id": "chatcmpl-stub", "object": "chat.completion.chunk", "model": "Qwen3.8-Max",
     "choices": [{"index": 0, "delta": {"reasoning_content": "先想一下"}, "finish_reason": None}]},
    {"id": "chatcmpl-stub", "object": "chat.completion.chunk", "model": "Qwen3.8-Max",
     "choices": [{"index": 0, "delta": {"content": "你好，"}, "finish_reason": None}]},
    {"id": "chatcmpl-stub", "object": "chat.completion.chunk", "model": "Qwen3.8-Max",
     "choices": [{"index": 0, "delta": {"content": "我是桩。"}, "finish_reason": "stop"}],
     "usage": {"prompt_tokens": 5, "completion_tokens": 6, "total_tokens": 11}},
]


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        sys.stderr.write("[stub] %s %s\n" % (self.command, self.path.split("?")[0]))

    def _json(self, obj, code=200):
        raw = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self):
        path = self.path.split("?")[0]
        if path.startswith("/algo/api/v2/model/list"):
            # 签名头必须真的到了，否则这条链路等于没验
            if not self.headers.get("Authorization", "").startswith("Bearer COSY."):
                return self._json({"message": "missing COSY signature"}, 403)
            return self._json(CATALOG)
        if path == "/api/v1/userinfo":
            return self._json({"id": "u-1", "name": "桩账号"})
        if path == "/api/v2/quota/usage":
            return self._json(QUOTA)
        if path == "/api/v2/user/plan":
            return self._json({"plan_tier_name": "Pro"})
        if path.startswith("/sash/api/v1/me/campaigns"):
            return self._json({"campaigns": [
                {"campaignId": "c-1", "actionType": "CLAIM_BENEFIT", "claimStatus": "CLAIMED"}]})
        return self._json({"message": "not found"}, 404)

    def do_POST(self):
        path = self.path.split("?")[0]
        length = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(length) if length else b""
        if path.endswith("/agent_chat_generation"):
            if not self.headers.get("Authorization", "").startswith("Bearer COSY."):
                return self._json({"message": "missing COSY signature"}, 403)
            # 上游 Encode=1 的自定义编码体：这里只确认非空且不是明文 JSON
            assert body and not body.startswith(b"{"), "请求体应已编码"
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Transfer-Encoding", "chunked")
            self.end_headers()
            for chunk in CHUNKS:
                envelope = {"statusCodeValue": 200, "body": json.dumps(chunk)}
                self._chunk("data: " + json.dumps(envelope) + "\n\n")
            self._chunk("data: [DONE]\n\n")
            self._chunk(None)
            return
        if path.endswith("/claim"):
            return self._json({"status": "CLAIMED", "benefit": {"amount": 100}})
        return self._json({"message": "not found"}, 404)

    def _chunk(self, payload):
        if payload is None:
            self.wfile.write(b"0\r\n\r\n")
            return
        raw = payload.encode()
        self.wfile.write(b"%x\r\n%s\r\n" % (len(raw), raw))


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 7899
    ThreadingHTTPServer(("127.0.0.1", port), Handler).serve_forever()
