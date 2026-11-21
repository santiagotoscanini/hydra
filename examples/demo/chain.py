# Prints the request chain from a mocksvc reply on stdin.
import json, sys

def walk(r):
    s = f"{r['service']}(slot {r['slot']}, :{r['port']})"
    for u in (r.get("upstreams") or {}).values():
        s += " -> " + (walk(u["reply"]) if u.get("reply") else "ERROR " + u.get("error", ""))
    return s

print(walk(json.load(sys.stdin)))
