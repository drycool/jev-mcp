#!/usr/bin/env python3
"""Drive the compiled jev-mcp binary over stdio exactly as an MCP client would.

This is the end-to-end check: the Go tests use an HTTP stub, so only this run
proves the binary speaks the protocol to the real router.
"""
import json
import subprocess
import sys
import time

BIN = "/home/dry/jev-mcp/jev-mcp"


def main() -> int:
    # stderr goes to a file, not a pipe: reading a pipe from a process that is
    # still alive blocks until it exits, which is how this script deadlocked the
    # first time it ran and hid the very output it was meant to show.
    stderr_log = open("/tmp/jev_mcp_stderr.log", "w", encoding="utf-8")
    proc = subprocess.Popen(
        [BIN],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=stderr_log,
        text=True,
        encoding="utf-8",
        bufsize=1,
    )

    def send(obj) -> None:
        proc.stdin.write(json.dumps(obj, ensure_ascii=False) + "\n")
        proc.stdin.flush()

    def read() -> dict:
        line = proc.stdout.readline()
        if not line:
            err = proc.stderr.read()
            raise SystemExit(f"server closed the pipe. stderr:\n{err}")
        return json.loads(line)

    try:
        send({"jsonrpc": "2.0", "id": 1, "method": "initialize",
              "params": {"protocolVersion": "2025-06-18",
                         "capabilities": {},
                         "clientInfo": {"name": "smoke", "version": "1"}}})
        init = read()
        info = init["result"]
        print(f"1. initialize      -> protocol={info['protocolVersion']} "
              f"server={info['serverInfo']['name']} v{info['serverInfo']['version']}")
        print(f"   capabilities    -> {list(info['capabilities'].keys())}")

        send({"jsonrpc": "2.0", "method": "notifications/initialized"})

        send({"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
        tools = read()["result"]["tools"]
        print(f"2. tools/list      -> {len(tools)}: {[t['name'] for t in tools]}")

        send({"jsonrpc": "2.0", "id": 3, "method": "tools/call",
              "params": {"name": "jev_health", "arguments": {}}})
        health = read()["result"]["content"][0]["text"]
        print("3. jev_health      ->")
        for line in health.splitlines():
            print(f"     {line}")

        t0 = time.perf_counter()
        send({"jsonrpc": "2.0", "id": 4, "method": "tools/call",
              "params": {"name": "jev_query",
                         "arguments": {"query": "момент затяжки болтов головки блока цилиндров"}}})
        routed = read()["result"]
        elapsed = (time.perf_counter() - t0) * 1000
        text = routed["content"][0]["text"]
        print(f"4. jev_query (execute=false) -> isError={routed.get('isError', False)}, "
              f"round trip через MCP={elapsed:.0f} мс")
        print("   " + "\n   ".join(text.splitlines()[-3:]))

        t0 = time.perf_counter()
        send({"jsonrpc": "2.0", "id": 5, "method": "tools/call",
              "params": {"name": "jev_query",
                         "arguments": {"query": "какой момент затяжки болтов головки блока цилиндров",
                                       "execute": True}}})
        answered = read()["result"]
        elapsed = (time.perf_counter() - t0) * 1000
        text = answered["content"][0]["text"]
        body = text.split("\n———")[0]
        print(f"5. jev_query (execute=true)  -> isError={answered.get('isError', False)}, "
              f"round trip через MCP={elapsed:.0f} мс")
        print(f"   ответ ({len(body)} символов): {body[:200]}...")

        send({"jsonrpc": "2.0", "id": 6, "method": "tools/call",
              "params": {"name": "jev_stats", "arguments": {}}})
        stats = read()["result"]["content"][0]["text"]
        print("6. jev_stats       ->")
        for line in stats.splitlines()[:8]:
            print(f"     {line}")

        send({"jsonrpc": "2.0", "id": 7, "method": "tools/call",
              "params": {"name": "jev_query", "arguments": {}}})
        bad = read()["result"]
        print(f"7. пустой query    -> isError={bad.get('isError', False)}: "
              f"{bad['content'][0]['text']}")
        return 0
    finally:
        proc.stdin.close()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()
        print(f"процесс завершился, код {proc.returncode}")


if __name__ == "__main__":
    sys.exit(main())
