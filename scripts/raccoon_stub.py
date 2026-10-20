import json
import http.server
import socketserver


class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def _send(self, code, body):
        b = body.encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

    def do_GET(self):
        p = self.path.split("?")[0]
        if p == "/model_catalog":
            return self._send(200, json.dumps({"code": 0, "data": {"categories": [{"type": "chat", "models": [
                {"name": "raccoon-chat-ml-5-5", "display_name": "Raccoon Chat", "tags": ["thinking"],
                 "context_window": "180000", "max_tokens": 80000, "billing_multiplier": 1, "visible": True}]}]}}))
        if p == "/balance":
            return self._send(200, '{"code":0,"data":{"available_points":777,"daily_points":30}}')
        if p == "/api/web/office/v3/setting_info":
            return self._send(200, '{"code":0,"data":{}}')
        return self._send(404, "{}")

    def do_POST(self):
        p = self.path.split("?")[0]
        body = self.rfile.read(int(self.headers.get("content-length") or 0))
        if p == "/refresh":
            return self._send(200, '{"code":0,"data":{"access_token":"fresh","refresh_token":"fresh-r","user_id":"u-smoke"}}')
        if p != "/chat/completions":
            return self._send(404, "{}")
        req = json.loads(body or b"{}")
        if not req.get("stream"):
            return self._send(200, json.dumps({
                "id": "c1", "object": "chat.completion", "model": "internal-name",
                "choices": [{"index": 0, "message": {"role": "assistant", "content": "non-stream-ok"}, "finish_reason": "stop"}],
                "usage": {"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8}}))
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()
        frames = [
            {"delta": {"role": "assistant", "content": ""}},
            {"delta": {"reasoning_content": "想"}},
            {"delta": {"content": "流式"}},
            {"delta": {}, "finish_reason": "stop", "usage": {"prompt_tokens": 5, "completion_tokens": 3}},
        ]
        for f in frames:
            choice = {"index": 0}
            choice.update(f)
            chunk = {"id": "c1", "object": "chat.completion.chunk", "model": "internal-name", "created": 1, "choices": [choice]}
            self.wfile.write(("data: " + json.dumps(chunk) + "\n\n").encode())
            self.wfile.flush()
        self.wfile.write(b"data: [DONE]\n\n")
        self.wfile.flush()


socketserver.TCPServer.allow_reuse_address = True
socketserver.TCPServer(("127.0.0.1", 17901), H).serve_forever()
