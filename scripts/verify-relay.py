#!/usr/bin/env python3
"""对 docker 里跑的 relay 做端到端验证（上游由 verify-upstream.py 模拟）。"""
import base64
import json
import sys
import time
import urllib.error
import urllib.request

RELAY = "http://127.0.0.1:18080"
UP = "http://127.0.0.1:9001"
ADMIN = "test-admin"

FAILS = []


def req(url, method="GET", body=None, headers=None, cookie=None):
    data = None
    hdrs = dict(headers or {})
    if body is not None:
        data = json.dumps(body).encode()
        hdrs["Content-Type"] = "application/json"
    if cookie:
        hdrs["Cookie"] = cookie
    r = urllib.request.Request(url, data=data, headers=hdrs, method=method)
    try:
        with urllib.request.urlopen(r, timeout=60) as resp:
            raw = resp.read()
            return resp.status, dict(resp.headers), raw
    except urllib.error.HTTPError as e:
        return e.code, dict(e.headers), e.read()
    except Exception as e:  # noqa
        return 0, {}, str(e).encode()


def jbody(raw):
    try:
        return json.loads(raw or b"{}")
    except Exception:
        return {"_raw": raw.decode("utf-8", "replace")}


def check(name, cond, detail=""):
    if cond:
        print(f"  PASS  {name}")
    else:
        print(f"  FAIL  {name}  {detail}")
        FAILS.append(name)


def make_jwt(uid, iss="xiaohuanxiong.com/iam", exp_offset=3 * 3600):
    header = base64.urlsafe_b64encode(json.dumps({"alg": "HS256", "typ": "JWT"}).encode()).rstrip(b"=")
    payload = base64.urlsafe_b64encode(json.dumps({
        "sub": uid, "iss": iss, "exp": int(time.time()) + exp_offset, "name": uid,
    }).encode()).rstrip(b"=")
    return (header + b"." + payload + b".sig").decode()


def login():
    status, hdrs, raw = req(RELAY + "/api/login", "POST", {"token": ADMIN})
    check("管理员登录", status == 200, raw)
    cookies = hdrs.get("Set-Cookie", "")
    return cookies.split(";")[0] if cookies else ""


def main():
    cookie = login()
    if not cookie:
        print("登录失败，终止")
        return 1

    # 1. 创建 API Key
    status, _, raw = req(RELAY + "/api/keys", "POST", {"name": "verify"}, cookie=cookie)
    key = jbody(raw).get("key", "")
    check("创建 API Key", status == 200 and key.startswith("rc-"), raw)

    # 2. 添加账号（JWT 由本地构造，上游不验签）
    status, _, raw = req(RELAY + "/api/auth/manual", "POST", {
        "name": "verify-1", "token": make_jwt("pu-verify-1"),
    }, cookie=cookie)
    body = jbody(raw)
    check("手工添加账号", status == 200 and body.get("ok"), raw)

    # 3. 会话标识：同一段对话两轮 -> 同一个 session，不同 turn
    req(UP + "/__reset", "POST", {})
    req(RELAY + "/v1/chat/completions", "POST", {
        "model": "sn-test-model", "messages": [{"role": "user", "content": "第一问"}],
    }, headers={"Authorization": "Bearer " + key})
    req(RELAY + "/v1/chat/completions", "POST", {
        "model": "sn-test-model", "messages": [
            {"role": "user", "content": "第一问"},
            {"role": "assistant", "content": "答"},
            {"role": "user", "content": "第二问"},
        ],
    }, headers={"Authorization": "Bearer " + key})
    _, _, raw = req(UP + "/__state")
    st = jbody(raw)
    hs = st.get("chat_headers", [])
    check("两轮请求都到达上游", len(hs) == 2, hs)
    if len(hs) == 2:
        check("同一对话共享 Session-ID", hs[0]["session"] == hs[1]["session"] and hs[0]["session"],
              hs)
        check("新消息形成新 Turn-ID", hs[0]["turn"] != hs[1]["turn"], hs)

    # 3.1 显式会话头：不同首条消息也共用一个 session
    req(UP + "/__reset", "POST", {})
    for text in ("甲", "乙"):
        req(RELAY + "/v1/chat/completions", "POST", {
            "model": "sn-test-model", "messages": [{"role": "user", "content": text}],
        }, headers={"Authorization": "Bearer " + key, "X-Session-Id": "conv-verify"})
    _, _, raw = req(UP + "/__state")
    hs = jbody(raw).get("chat_headers", [])
    check("显式会话头跨轮稳定", len(hs) == 2 and hs[0]["session"] == hs[1]["session"] and hs[0]["session"],
          hs)

    # 3.2 工具续轮复用 Turn
    req(UP + "/__reset", "POST", {})
    req(RELAY + "/v1/chat/completions", "POST", {
        "model": "sn-test-model", "messages": [{"role": "user", "content": "查天气"}],
    }, headers={"Authorization": "Bearer " + key})
    req(RELAY + "/v1/chat/completions", "POST", {
        "model": "sn-test-model", "messages": [
            {"role": "user", "content": "查天气"},
            {"role": "assistant", "content": "", "tool_calls": [
                {"id": "c1", "type": "function", "function": {"name": "w", "arguments": "{}"}}]},
            {"role": "tool", "tool_call_id": "c1", "content": "晴"},
        ],
    }, headers={"Authorization": "Bearer " + key})
    _, _, raw = req(UP + "/__state")
    hs = jbody(raw).get("chat_headers", [])
    check("工具续轮复用 Turn", len(hs) == 2 and hs[0]["turn"] == hs[1]["turn"] and hs[0]["turn"], hs)

    # 4. 字符串错误码 -> 积分耗尽，号被停用
    req(UP + "/__mode", "POST", {"chat_mode": "named_quota"})
    status, _, raw = req(RELAY + "/v1/chat/completions", "POST", {
        "model": "sn-test-model", "messages": [{"role": "user", "content": "hi"}],
    }, headers={"Authorization": "Bearer " + key})
    check("积分不足对外回 402", status == 402, f"{status} {raw[:200]}")
    _, _, raw = req(RELAY + "/api/accounts", cookie=cookie)
    accs = jbody(raw).get("accounts", jbody(raw).get("data", []))
    if isinstance(accs, dict):
        accs = accs.get("accounts", [])
    exhausted = [a for a in accs if a.get("status") == "exhausted"]
    check("字符串码 insufficient_points 触发积分耗尽停号", len(exhausted) == 1,
          [(a.get("name"), a.get("status")) for a in accs])

    # 5. 流内字符串错误帧同样识别
    req(UP + "/__mode", "POST", {"chat_mode": "stream_quota"})
    if accs:
        acc_id = accs[0].get("id")
        req(RELAY + f"/api/accounts/{acc_id}/uncool", "POST", {}, cookie=cookie)
    status, _, raw = req(RELAY + "/v1/chat/completions", "POST", {
        "model": "sn-test-model", "messages": [{"role": "user", "content": "hi"}],
    }, headers={"Authorization": "Bearer " + key})
    check("流内字符串码不被当成空响应", status == 402, f"{status} {raw[:300]}")

    # 6. 内容策略拦截：不换号、不冷却，回 400
    req(UP + "/__mode", "POST", {"chat_mode": "content_block"})
    if accs:
        req(RELAY + f"/api/accounts/{accs[0].get('id')}/uncool", "POST", {}, cookie=cookie)
    status, _, raw = req(RELAY + "/v1/chat/completions", "POST", {
        "model": "sn-test-model", "messages": [{"role": "user", "content": "hi"}],
    }, headers={"Authorization": "Bearer " + key})
    check("内容策略拦截回 400", status == 400, f"{status} {raw[:200]}")
    # 内容拦截不该停号：账号应仍为 online
    _, _, raw = req(RELAY + "/api/accounts", cookie=cookie)
    now_accs = jbody(raw).get("accounts", [])
    check("内容拦截不冷却账号", all(a.get("status") == "online" for a in now_accs),
          [(a.get("name"), a.get("status"), a.get("coolReason")) for a in now_accs])

    # 7. 模型清单：隐藏模型不广告，可见模型带倍率
    req(UP + "/__mode", "POST", {"chat_mode": "ok"})
    status, _, raw = req(RELAY + "/v1/models", headers={"Authorization": "Bearer " + key})
    body = jbody(raw)
    ids = [m.get("id") for m in body.get("data", [])]
    check("隐藏模型不进 /v1/models", "raccoon-8c4485" not in ids, ids)
    check("可见模型在 /v1/models", "sn-test-model" in ids, ids)
    entry = next((m for m in body.get("data", []) if m.get("id") == "sn-test-model"), {})
    check("模型条目带倍率", entry.get("pointsMultiplier") == 0.5, entry)

    # 8. 签到：以账单为准，记真实入账积分
    # 显式复位：上一轮若改过 bills_points，这里不复位会读到旧值。
    req(UP + "/__mode", "POST", {"bills_points": 300, "include_today_grant": True})
    req(UP + "/__reset", "POST", {})
    if accs:
        req(RELAY + f"/api/accounts/{accs[0].get('id')}/checkin", "POST", {}, cookie=cookie)
    status, _, raw = req(RELAY + "/api/checkin/history?limit=5", cookie=cookie)
    hist = jbody(raw)
    items = hist.get("records", hist.get("items", hist.get("data", [])))
    ok_credit = any(r.get("credit") == 300 for r in items if isinstance(r, dict))
    check("签到按账单记入 300 积分", ok_credit, items[:3])

    _, _, raw = req(UP + "/__state")
    st = jbody(raw)
    check("签到调用了发放触发器 setting_info", st.get("setting_hits", 0) >= 1, st)
    check("签到调用了账单核对", st.get("bills_hits", 0) >= 1, st)

    # 9. v3 setting_info 被整体拦截时，签到仍能退到 v2 成功
    req(UP + "/__mode", "POST", {"v3_blocked": True, "include_today_grant": True, "bills_points": 150})
    req(UP + "/__reset", "POST", {})
    if accs:
        status, _, raw = req(RELAY + f"/api/accounts/{accs[0].get('id')}/checkin", "POST", {}, cookie=cookie)
        body = jbody(raw)
        check("v3 被拦截时签到仍成功（走 v2）", status == 200 and body.get("ok"),
              f"{status} {body}")
        check("v2 通道下账单仍记入 150", body.get("credit") == 150 or body.get("verified") is True,
              body)
    req(UP + "/__mode", "POST", {"v3_blocked": False})

    # 10. 429 分流：真限流只罚模型、不停号；配额型 429 判积分耗尽
    req(UP + "/__mode", "POST", {"chat_mode": "429"})
    status, _, raw = req(RELAY + "/v1/chat/completions", "POST", {
        "model": "sn-test-model", "messages": [{"role": "user", "content": "hi"}],
    }, headers={"Authorization": "Bearer " + key})
    check("真限流对外回 429", status == 429, f"{status} {raw[:200]}")
    _, _, raw = req(RELAY + "/api/accounts", cookie=cookie)
    now_accs = jbody(raw).get("accounts", [])
    not_exhausted = all(a.get("status") != "exhausted" for a in now_accs)
    check("真限流不判积分耗尽", not_exhausted,
          [(a.get("name"), a.get("status")) for a in now_accs])

    req(UP + "/__mode", "POST", {"chat_mode": "429_quota"})
    if accs:
        req(RELAY + f"/api/accounts/{accs[0].get('id')}/uncool", "POST", {}, cookie=cookie)
    status, _, raw = req(RELAY + "/v1/chat/completions", "POST", {
        "model": "sn-test-model", "messages": [{"role": "user", "content": "hi"}],
    }, headers={"Authorization": "Bearer " + key})
    check("配额型 429 对外回 402", status == 402, f"{status} {raw[:200]}")
    _, _, raw = req(RELAY + "/api/accounts", cookie=cookie)
    now_accs = jbody(raw).get("accounts", [])
    exhausted = [a for a in now_accs if a.get("status") == "exhausted"]
    check("配额型 429 停号到下次签到", len(exhausted) == 1,
          [(a.get("name"), a.get("status")) for a in now_accs])
    req(UP + "/__mode", "POST", {"chat_mode": "ok"})

    print()
    if FAILS:
        print("失败项：", FAILS)
        return 1
    print("全部通过")
    return 0


if __name__ == "__main__":
    sys.exit(main())
