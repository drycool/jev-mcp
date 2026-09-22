# jev-mcp

An MCP server that exposes the [Jev router](../Jev) as tools, so any MCP-capable agent —
Hermes, Codex, OpenCode — can send a question *through* the router instead of reaching
for a vector store or the knowledge graph itself.

## Why a router rather than another search tool

Jev already orders its tiers by cost and reports which one answered. A search tool makes
the caller guess how expensive its own question will be; this one makes the choice and
says what it cost. Measured on the Pi5 + RTX 3060 installation, 2026-09-22, Espero manual
lookups:

| shape | measured |
|---|---|
| command (tier 1, no LLM) | 2 ms |
| exact FTS5 hit, context only (`execute=false`) | 9–39 ms |
| no local hit, graph tier parked | 157 ms |
| agent-tier synthesis (`execute=true`) | 10.1 s |
| the same question through LightRAG's graph directly | 37.0 s |

The last two rows are why the router is worth a hop: for a manual lookup the graph was both
**3.7× slower and off-topic** (it answered about an Argon ONE V3 Raspberry Pi case), because
its knowledge base mixes the Espero manual with Raspberry Pi accessory notes.

## Build

```bash
cd jev-mcp
make build        # -> ./jev-mcp  (Go stdlib only, no dependencies)
make test         # 23 tests
make smoke        # drives the compiled binary over stdio against the live router
```

Go 1.22+, stdlib only. Nothing to install.

## Tools

| tool | what it does |
|---|---|
| `jev_query` | Routes a question; returns the answer **plus provenance** — strategy, target agent, measured latency, degradation state, and the `decision_id` that makes the answer labellable. `execute=false` (default) returns routing and local context in milliseconds; `execute=true` also synthesises an answer through the agent tier and costs seconds. |
| `jev_health` | Which tiers are up and which configuration is in force (`lightrag_enabled`, `vector_timeout_s`, LLM host, shadow mode, labelling settings). Call this first when a call fails, hangs, or comes back empty, so a *disabled* tier is not mistaken for a broken one. |
| `jev_stats` | Counters since start, every one of them including the zeros. An all-zero counter set is the evidence that nothing is calling the router. |
| `jev_feedback` | Records whether a `jev_query` answer was actually usable, against the `decision_id` that came with it. This is the only ground truth the system can have, and the one call worth making when the answer was **wrong**. |

Every `jev_query` result ends with a provenance block, so the caller can tell a 9 ms
exact hit from a 10 s synthesis without guessing:

```
25 Н·м в несколько этапов
———
strategy: exact_fts (0.95) · target: general_agent · elapsed: 10107 ms · degraded: false
intent: exact_search · domain: general · keywords: момент, затяжки, болтов, головки
lightrag: mode=skip required=false
decision_id: 6bbbf8b31b4b4a2a8072665b377c0408 (report a verdict with jev_feedback)
```

## The labelling loop

An answer is not a training example until someone says whether it was right, and the router
cannot be that someone. So the loop closes through the caller:

```
jev_query  ->  answer + decision_id
                    |
             you use the answer and find out whether it worked
                    |
jev_feedback(decision_id, accepted | partial | rejected, query, comment)
```

`accepted` means usable as given, `partial` means it needed correction or more work,
`rejected` means wrong. There is deliberately no `unknown`: an abstention carries no signal
and would only inflate the label count. `source` is `agent` (the default), `human` or
`script`, and a human verdict outranks an agent's when the dataset is read.

**Pass `query` with the question you asked.** The router stores only a hash of it, so a
verdict without its question can never be re-checked by anyone else — the reader can see what
was answered but not what was asked. You have the question at the moment you judge; this is
the only point at which it can be captured without turning on raw-query logging.

Report the **wrong** answers especially. A log of accepted answers cannot calibrate a
threshold or train anything — the failures are the only part with information in it.

Verdicts are append-only, so reporting a correction later is both expected and safe; the
tool tells you when a verdict replaces an earlier one, and warns when the `decision_id`
labels nothing (`known_decision: false`), which is the failure mode that would quietly
invalidate a dataset.

See the router's README for the storage layout and for
`scripts/label_coverage.py`, which reports coverage and whether there is enough ground truth
to act on yet.

## Registering with clients

**Hermes** (`~/.hermes/config.yaml`, under `mcp_servers`) — or `hermes mcp add`:

```yaml
mcp_servers:
  jev:
    command: /home/dry/jev-mcp/jev-mcp
    args: []
    enabled: true
```

Verify without starting a session:

```bash
hermes mcp test jev      # ✓ Connected (151ms)  ✓ Tools discovered: 3
```

**Codex** (`~/.codex/config.toml`):

```toml
[mcp_servers.jev]
command = "/home/dry/jev-mcp/jev-mcp"
args = []
```

**OpenCode** (`~/.config/opencode/opencode.jsonc`):

```json
{
  "mcp": {
    "jev": {
      "type": "local",
      "command": ["/home/dry/jev-mcp/jev-mcp"],
      "enabled": true
    }
  }
}
```

One server, three clients: MCP is the interface, so no per-agent plugin is needed.

## Configuration

| variable | default | meaning |
|---|---|---|
| `JEV_URL` | `http://127.0.0.1:8030` | router base URL |
| `JEV_TIMEOUT_S` | `30` | budget for one tool call; `jev_query`'s `timeout_s` argument overrides it per call |

An invalid `JEV_TIMEOUT_S` is ignored with a note on stderr rather than preventing startup:
a typo in an environment variable should not leave the agent with no router at all.

## Pitfalls this server was built around

- **A numeric JSON-RPC id must come back as a number.** Ids are carried as `json.RawMessage`
  and echoed verbatim; re-encoding through `interface{}` is how servers start answering
  `"id": null` and clients start timing out.
- **Notifications are never answered**, including for methods this server does not know.
  A reply to a notification desynchronises clients that count responses.
- **Tool failures are content, not protocol errors.** A dead router comes back as
  `isError: true` with the status in the text, so the model can read it and adapt. As a
  JSON-RPC error the message would be stripped by the transport.
- **Nothing but protocol frames goes to stdout.** A stray `fmt.Println` on stdout is an
  unexplained client failure; diagnostics go to stderr.
- **The empty answer is explained.** `execute=false` legitimately returns no answer, and an
  empty string reads as a broken tool, so the result says which of the two happened.
- **jq's `//` treats `false` as absent.** A validation script of ours printed
  `lightrag_enabled=?` because it used `(.lightrag_enabled // "?")` — precisely when the
  value was `false`, the one value it existed to report. Caught by running it, not reading it.

## Layer separation

This server talks HTTP to the router and knows nothing about tiers, models or GPUs; the
router knows nothing about MCP. The router's own infrastructure concerns — the GPU node's
idle watchdog, leases, power management — are documented in the `devops/idle-gpu-node-leasing`
skill, not here.
