#!/usr/bin/env python3
"""Drive two chat sockets end to end, without a browser.

Signs in as two seeded accounts, mints a ClientId for each (ADR 18), opens both
sockets and checks that what one sends arrives at the other.

Usage:  python3 chatcheck.py [domain] [loopback-ip] [chat-id]
"""
import asyncio, json, ssl, socket, sys, urllib.request

DOMAIN = sys.argv[1] if len(sys.argv) > 1 else "example.com"
IP     = sys.argv[2] if len(sys.argv) > 2 else "127.0.0.2"
CHAT   = sys.argv[3] if len(sys.argv) > 3 else "2"
HOST   = f"webmail.{DOMAIN}"

ctx = ssl.create_default_context(cafile="certs/webmail-local-ca.crt")
# Resolve the Federation hostname to its loopback address, so this runs whether
# or not /etc/hosts has the entries the browser needs.
_gai = socket.getaddrinfo
socket.getaddrinfo = lambda h, *a, **k: _gai(IP, *a, **k) if h == HOST else _gai(h, *a, **k)
import websockets


def post(path, body, tok=None):
    head = {"Content-Type": "application/json"}
    if tok:
        head["Authorization"] = "Bearer " + tok
    req = urllib.request.Request(f"https://{HOST}{path}", json.dumps(body).encode(), head, method="POST")
    return json.load(urllib.request.urlopen(req, context=ctx))


async def expect(ws, kind, timeout=5):
    """Read until a frame of this type arrives. Frames interleave, so a bare
    recv() will often hand back somebody else's presence push instead."""
    while True:
        frame = json.loads(await asyncio.wait_for(ws.recv(), timeout))
        if frame.get("type") == kind:
            return frame


async def main():
    a, b = f"bob@{DOMAIN}", f"carol@{DOMAIN}"
    ta = post("/api/v1/auth/token", {"email": a, "password": "password"})["value"]
    tb = post("/api/v1/auth/token", {"email": b, "password": "password"})["value"]
    ca = post("/api/v1/chat/session", {}, ta)["clientId"]
    cb = post("/api/v1/chat/session", {}, tb)["clientId"]

    async with websockets.connect(f"wss://{HOST}/ws/{ca}", ssl=ctx) as wa, \
               websockets.connect(f"wss://{HOST}/ws/{cb}", ssl=ctx) as wb:
        for ws, who in ((wa, a), (wb, b)):
            await ws.send(json.dumps({"type": "hello"}))
            print(f"ready    {who} userId={(await expect(ws, 'ready'))['userId']}")

        await wa.send(json.dumps({"type": "typing", "chatId": CHAT, "on": True}))
        t = await expect(wb, "typing")
        print(f"typing   {b} sees userId={t['userId']} on chatId={t['chatId']}")

        await wa.send(json.dumps({"type": "message", "chatId": CHAT, "body": "socket check", "id": "c1"}))
        m = await expect(wb, "message")
        print(f"message  {b} sees {m['message']['body']!r} from {m['message']['senderEmail']}")
        print("\nOK. The chatId differs per side because each user reads their own projection (ADR 3).")

asyncio.run(main())
