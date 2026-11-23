#!/usr/bin/env python3
"""本地小浣熊上游模拟器：只实现 docker 验证需要的端点。

用固定端口 9001，让容器里的 relay 把 UPSTREAM_BASE 指过来。
"""
import json
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse, parse_qs

STATE = {
    "chat_mode": "ok",       # ok | named_quota | stream_quota | content_block | 429 | context
    "chat_headers": [],      # 记录每次对话请求的头
    "chat_bodies": [],
    "setting_hits": 0,
    "bills_hits": 0,
    "include_today_grant": True,
    "bills_points": 300,
    "iam_confirm_needed": False,
    "v3_blocked": False,     # 模拟 v3 setting_info 被网关整体拦截（对所有凭证 403）
}


def china_today():
    return time.strftime("%Y-%m-%d", time.gmtime(time.time() + 8 * 3600))


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        sys.stderr.write("[upstream] " + fmt % args + "\n")

    def _json(self, status, obj, extra_headers=None):
        raw = json.dumps(obj, ensure_ascii=False).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        for k, v in (extra_headers or {}).items():
            self.send_header(k, v)
        self.end_headers()
        self.wfile.write(raw)

    def _sse(self, frames):
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Connection", "close")
        self.end_headers()
        for frame in frames:
            self.wfile.write(f"data: {frame}\n\n".encode("utf-8"))
            self.wfile.flush()
        self.wfile.write(b"data: [DONE]\n\n")
        self.wfile.flush()
        self.close_connection = True

    def do_GET(self):
        path = urlparse(self.path).path
        if path == "/__state":
            self._json(200, STATE)
            return
        if path == "/api/web/office/v3/setting_info":
            if STATE["v3_blocked"]:
                # 上游对 v3 的整体拦截：任何凭证都 403，响应体没有业务信息。
                self.send_response(403)
                self.send_header("X-TT-System-Error", "3")
                self.send_header("Content-Length", "0")
                self.end_headers()
                return
            STATE["setting_hits"] += 1
            self._json(200, {"code": 0, "data": {"available_points": 4321,
                                                 "point_grant_popups": [{"points": STATE["bills_points"]}]}})
            return
        if path == "/api/web/office/v2/setting_info":
            STATE["setting_hits"] += 1
            self._json(200, {"code": 0, "data": {"available_points": 4321,
                                                 "point_grant_popups": [{"points": STATE["bills_points"]}]}})
            return
        if path == "/api/web/org/user/office/v1/setting_info":
            STATE["setting_hits"] += 1
            self._json(200, {"code": 0, "data": {"available_points": 4321}})
            return
        if path == "/api/web/points/v2/bills":
            STATE["bills_hits"] += 1
            items = []
            if STATE["include_today_grant"]:
                at = time.strftime("%Y-%m-%dT%H:%M:%S+08:00", time.gmtime(time.time() + 8 * 3600))
                items.append({"biz_type": "daily_grant", "event_name": "每日积分发放",
                              "points": STATE["bills_points"], "created_at": at})
            self._json(200, {"code": 0, "data": {"items": items, "has_more": False}})
            return
        if path == "/api/web/llm/v2/model_catalog":
            self._json(200, {"code": 0, "data": {"categories": [{"type": "chat", "models": [
                {"name": "sn-test-model", "display_name": "Test Model",
                 "params": {"context_window": 1000, "max_tokens": 100},
                 "billing_multiplier": 1, "billing_effective_multiplier": 0.5},
                {"name": "raccoon-8c4485", "display_name": "Raccoon-Work", "visible": False},
            ]}]}})
            return
        if path == "/api/web/auth/v1/user_info":
            if STATE["iam_confirm_needed"]:
                self._json(200, {"code": 200901, "message": "iam confirm required"})
                return
            self._json(200, {"code": 0, "data": {"id": "7160938", "name": "测试用户"}})
            return
        if path == "/api/web/points/v1/balance":
            self.send_response(403)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        self._json(404, {"code": 404, "message": "not found " + path})

    def do_POST(self):
        path = urlparse(self.path).path
        length = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(length) if length else b""
        # 测试控制面：改模拟器行为与重置记录
        if path == "/__mode":
            data = json.loads(body or b"{}")
            for key, value in data.items():
                if key in STATE:
                    STATE[key] = value
            self._json(200, {"ok": True})
            return
        if path == "/__reset":
            STATE["chat_headers"].clear()
            STATE["chat_bodies"].clear()
            STATE["setting_hits"] = 0
            STATE["bills_hits"] = 0
            self._json(200, {"ok": True})
            return
        if path == "/api/web/llm/v2/chat/completions":
            STATE["chat_headers"].append({
                "session": self.headers.get("X-RACCOON-Session-ID", ""),
                "turn": self.headers.get("X-RACCOON-Turn-ID", ""),
                "org": self.headers.get("X-Org-Code", ""),
                "client": self.headers.get("x-client-version", ""),
            })
            STATE["chat_bodies"].append(body.decode("utf-8", "replace"))
            mode = STATE["chat_mode"]
            if mode == "named_quota":
                self._json(400, {"error": {"code": "insufficient_points", "message": "积分不足"}})
                return
            if mode == "content_block":
                self._json(400, {"error": {"message": "request blocked by security policy"}})
                return
            if mode == "429":
                self._json(429, {"code": "rate_limit_exceeded", "message": "too many requests"})
                return
            if mode == "429_quota":
                self._json(429, {"code": "insufficient_points", "message": "积分不足，请充值后重试"})
                return
            if mode == "context":
                self._json(400, {"code": 11115, "message": "context too long"})
                return
            if mode == "stream_quota":
                self._sse([json.dumps({"error": {"code": "insufficient_points", "message": "积分余额不足"}},
                                      ensure_ascii=False)])
                return
            self._sse([
                json.dumps({"choices": [{"index": 0, "delta": {"role": "assistant", "content": ""}}]}),
                json.dumps({"choices": [{"index": 0, "delta": {"content": "你好"}}]}, ensure_ascii=False),
                json.dumps({"choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}]}),
                json.dumps({"choices": [], "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}}),
            ])
            return
        if path == "/api/web/auth/v1/iam/confirm":
            STATE["iam_confirm_needed"] = False
            self._json(200, {"code": 0, "data": {}})
            return
        if path == "/api/web/auth/v1/refresh":
            self._json(200, {"code": 0, "data": {"access_token": "new-access", "refresh_token": "new-refresh"}})
            return
        if path in ("/api/web/desktop/v1/login/points/grant", "/api/web/mobile/v1/login/points/grant"):
            self._json(200, {"code": 0, "data": {"granted": False}})
            return
        self._json(404, {"code": 404, "message": "not found " + path})


def main():
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 9001
    server = ThreadingHTTPServer(("0.0.0.0", port), Handler)
    print(f"upstream simulator listening on :{port}", flush=True)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    try:
        while True:
            time.sleep(3600)
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
