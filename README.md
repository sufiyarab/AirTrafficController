# Air Traffic Controller but Different

SOFE4790U Distributed Systems Fall 2026 final project.
Team: Sufiya Arab, Mashal Niazi, Issaq Momand, Collins Ehirim, Chimdike Nnadi.

A simulated air traffic control system where the airspace is split into
sectors, and every sector is run by its own 3-node Raft cluster
(hashicorp/raft). A replicated airspace registry decides which sector owns
each aircraft, and aircraft move between sectors through a 3-phase handoff
(begin, accept, commit), so no aircraft is ever lost or owned twice, even
while nodes crash or the network splits.

On top of that base design, the system adds:

- **Self-resizing sectors:** a busy sector splits into a standby cluster and merges back when traffic drops.
- **Live safety panel:** six safety rules are checked every second and shown on the radar.
- **Ghost aircraft detection:** three ground receivers cross-check every aircraft's claimed position, so a spoofed aircraft is flagged and refused clearances.
- **Conflict detection:** predicted losses of separation up to two minutes ahead, with a suggested climb or descent.
- **Security:** controller login with roles, and gRPC with mutual TLS between services.
- **Monitoring:** Prometheus metrics and Grafana dashboards.

Everything runs with one command in Docker: 21 Raft nodes (4 sectors, a
tower, the registry and a standby cluster, 3 nodes each), a Go gateway, a
Python aircraft simulator, 3 ground receivers and a React radar dashboard.

## Quick start

1. Install Docker Desktop and start it.
2. From the repository folder, run `docker compose up --build` (the first build takes a few minutes).
3. Open http://localhost:3000 to see the radar. Log in as `supervisor` / `supervisor-demo` to issue clearances.
4. Stop with `docker compose down`.

The sections below explain each feature and how to demo it.

## What you need

- **Docker Desktop** (or Docker Engine + Compose v2). That is all you need to
  build and run everything. The Go compiler, Node and Python all run inside
  the build containers (`golang:1.26-alpine`, `node:22-alpine`, `python:3.12-slim`).
- Optional, for editing with autocomplete and running `go test` / `go vet`
  outside Docker: Go 1.26+. Optional for dashboard hot reload: Node 22.
- Optional, for the experiment and comparison scripts: Python 3.10+
  (standard library only; `matplotlib` for the graphs).

**On Windows:** the `docker compose` commands work in PowerShell. The shell
scripts (`scripts/chaos.sh`, `scripts/gen-certs.sh`) and the commands that set
variables inline (like `SCENARIO=... docker compose up`) need Git Bash or WSL.
In PowerShell, set the variable first instead:
`$env:SCENARIO="scenarios/spoof_traffic.json"; docker compose up --build`.
Use `python` instead of `python3` if that is how Python is installed.

## Run it

```bash
docker compose up --build            # first build takes a few minutes
docker compose --profile maekawa up --build   # also start the Maekawa baseline
```

- Radar dashboard: http://localhost:3000
- Gateway state (JSON): http://localhost:8080/api/state
- With metrics: `docker compose --profile metrics up --build`, then
  Prometheus on http://localhost:9090 and Grafana on http://localhost:3001
  (dashboards are loaded automatically, see "Metrics and Grafana")

Stop with `docker compose down`. That also wipes the Raft logs, because they
live in each container's filesystem; `docker kill` / `docker start` keep them.

All 21 Raft nodes run the same Go image (`deploy/go.Dockerfile`, multi-stage:
full Go toolchain to build, small Alpine image to run). `NODE_ID`, `CLUSTER`
and `PEERS` decide which cluster a node belongs to.

## Fault injection

```bash
scripts/chaos.sh status                  # leader of every cluster
scripts/chaos.sh kill-leader sector-1    # crash the leader, watch a new one get elected
scripts/chaos.sh start sector-1-b        # bring the old leader back; it catches up from its log
scripts/chaos.sh partition sector-2-b    # disconnect a node from the network
scripts/chaos.sh heal sector-2-b
scripts/chaos.sh pause sector-3-a        # freeze a node (slow, not dead)
scripts/chaos.sh delay sector-3-a 200    # 200 ms latency with tc netem
scripts/chaos.sh clear sector-3-a
```

## Controller login

Watching the radar is open to everyone. Issuing a clearance needs a login: a
controller can only clear aircraft in its own sectors, a supervisor any
sector (`cmd/gateway/auth.go`). Demo accounts in `deploy/users.json`:

| Username | Password | Can clear |
|---|---|---|
| `ctrl-sector1` ... `ctrl-sector4` | `sector1-demo` ... `sector4-demo` | its sector |
| `ctrl-tower` | `tower-demo` | tower |
| `supervisor` | `supervisor-demo` | all sectors |

Demo passwords only. The file holds bcrypt hashes, never passwords; make a
new one with `echo -n 'new-password' | go run ./cmd/hashpw`.

```bash
TOKEN=$(curl -s localhost:8080/api/login -d '{"username":"ctrl-sector1","password":"sector1-demo"}' | jq -r .token)
curl -s localhost:8080/api/clearance -H "Authorization: Bearer $TOKEN" -d '{"callsign":"ATC001","instruction":"CLIMB FL350"}'
```

Without a token the clearance is refused with 401; for an aircraft in another
sector, 403. Tokens expire after 30 minutes. Set `JWT_SECRET` (32+ bytes) to
keep tokens valid across gateway restarts; `AUTH=off` turns login off.

## Ghost aircraft (spoofing) demo

Three ground receivers measure the range to every transmitter and report it
to the gateway, which checks it against the position each aircraft claims
(`cmd/gateway/verify.go`). A spoofed aircraft claims a normal track but
transmits from the spoofer's ground station, so the ranges disagree and it
turns `SUSPECT` (red, `?` on the radar) within about 3 ticks. Clearances to a
`SUSPECT` aircraft are refused with 409.

```bash
SCENARIO=scenarios/spoof_traffic.json docker compose up --build   # 6 real + 2 ghosts
SPOOF=3 docker compose up --build                                  # random fleet + 3 ghosts
docker kill atc-receiver-b-1     # coverage drops: some aircraft UNVERIFIED, none falsely SUSPECT
docker start atc-receiver-b-1
```

Tick the "coverage" box on the dashboard to draw each receiver's 200 NM
range. Tunables: `VERIFY_THRESHOLD_NM` (gateway, default 3),
`RECEIVER_NOISE_NM` (receivers, default 0.5).

## Self-resizing sectors (elastic airspace)

The gateway counts the aircraft in every sector once a second
(`cmd/gateway/elastic.go`). When a sector holds 40 or more aircraft for 3
checks in a row, it is split along its longer side: the new sector map is
committed to the registry, the idle standby cluster takes the new half, and
the aircraft in it move over through the normal 3-phase handoff. When the
standby sector and its neighbour together drop to 20 or fewer, the standby
sector is drained back and the cluster returns to STANDBY.

```bash
docker compose -f docker-compose.yml -f docker-compose.surge.yml up --build
```

60 aircraft appear together in sector-1: within a few seconds the
"elastic sectors" panel shows the split and the radar redraws the boundary,
and once the aircraft spread out the sectors merge back. A supervisor can
also split or merge by hand with the buttons in that panel.

| Setting (gateway) | Default | What it does |
|---|---|---|
| `ELASTIC_SPLIT_AT` | 40 | aircraft at or above which a sector splits |
| `ELASTIC_MERGE_AT` | 20 | combined aircraft at or below which a split sector merges back |
| `ELASTIC_SUSTAIN` | 3 | one-second checks the condition must hold |
| `ELASTIC_COOLDOWN_S` | 15 | pause after every split or merge |

Known limitation: after a split, only the supervisor can issue clearances in
the half on the standby cluster (see docs/collins_design.md, section 5).

## Safety-invariant panel

The "safety invariants" panel on the dashboard checks six rules every second
against what the clusters actually hold (`cmd/gateway/safety.go`):

| Rule | What is checked |
|---|---|
| One controller per aircraft | every tracked aircraft has exactly one owner in the registry, and that sector has airspace |
| One leader per term | no Raft cluster has two leaders in the same term |
| Clearances only from the owner | each clearance committed while its sector owned the aircraft (registry re-read after the commit), and every clearance record in a sector's log names that sector |
| No clearance to a ghost | no clearance commits for an aircraft that was already SUSPECT |
| No clearance lost | issued = committed + rejected + expired + pending, and every committed clearance is still in its sector's replicated state |
| Every loss of separation is alerted | an all-pairs check finds every pair closer than 5 NM and 1,000 ft right now; each one must already be on the conflict list |

Green means the rule held at every check since the gateway started; a broken
rule stays red and shows what broke it. The same data is at
`GET /api/safety`. Kill a leader or trigger a surge and the panel should stay
green. `go test ./cmd/gateway -run Safety` breaks each rule on purpose to show
the checks catch it.

## Conflict detection

The gateway predicts losses of separation (closer than 5 NM horizontally and
1,000 ft vertically at the same time) up to two minutes ahead
(`cmd/gateway/conflict.go`, once a second):

- Aircraft go into a grid of square cells as wide as 5 NM plus the distance
  two aircraft can close in two minutes, so a pair that can meet in time is
  always in the same or neighbouring cells. Only those pairs are checked, which
  is about O(n) instead of all n(n-1)/2 pairs.
- Each candidate pair is solved exactly: the time window when they are within
  5 NM (a quadratic in time) and the window when they are within 1,000 ft
  (from each aircraft's climb rate) must overlap inside the next 120 s.
- Each conflict gets a suggested resolution: a climb or descent of 2,000 ft or
  more for one of the two, checked against every other aircraft and kept
  between FL050 and FL410.

The radar draws a dashed amber line between a predicted pair (with the seconds
to the loss) and a solid red line once separation is lost. The "conflicts" panel
lists each one with a button that issues the suggested clearance through the
normal clearance path (login and sector rules apply).

The simulator flies committed clearances: the reply to each position report
(`ReportPositions` in `proto/atc.proto` returns `Instructions`) carries the
latest committed clearances for those aircraft, and the simulator climbs at
2,000 ft/min, turns at 3 degrees/s and changes speed at 2 kt/s to meet them.
To try it:

```bash
SCENARIO=scenarios/conflict_resolve.json docker compose up --build
```

A head-on pair is alerted within seconds and a crossing pair about two minutes
before they meet. Log in as `supervisor` and press the suggestion button.

| Setting | Default | What it does |
|---|---|---|
| `CONFLICT_LOOKAHEAD_S` (gateway) | 120 | look-ahead in seconds |
| `FOLLOW_CLEARANCES` (simulator) | 1 | 0 = aircraft ignore clearances |

Both can be set on the command line, e.g. `FOLLOW_CLEARANCES=0 docker compose up`.

## Record and replay traffic

The simulator can record every tick to a JSON Lines file and replay it later,
so the same traffic can be used for every run (`sim/recording.py`).

```bash
docker compose -f docker-compose.yml -f docker-compose.record.yml up --build   # records basic_traffic.json to sim/recordings/traffic.jsonl
docker compose down
docker compose -f docker-compose.yml -f docker-compose.replay.yml up --build   # replays it
docker compose logs simulator | tail                                           # "replay complete: N attempted, N acknowledged, 0 failed"
```

Run `docker compose down` between recording and replaying so the Raft nodes
start fresh and old sequence numbers do not reject the replayed reports.

## Metrics and Grafana

```bash
docker compose --profile metrics up --build
```

Open http://localhost:3001 (no login in the demo). Three dashboards are loaded
into the **ATC** folder, with the overview as the home page:

| Dashboard | What it answers |
|---|---|
| ATC Overview | Is the system healthy and how fast is it? Aircraft, nodes down, clusters without exactly one leader, safety invariants, predicted conflicts, ghost aircraft; aircraft per sector; handoffs and clearances per second; handoff and clearance latency (p50 / p95 / p99); Raft throughput |
| ATC Raft Clusters | Consensus health, with a cluster picker: the role of every node over time (follower / candidate / leader), leaders per cluster, elections, replication lag, log throughput, the gateway's Ready flag and the clearance buffer |
| ATC Elastic Sectors and Safety | Splits and merges: sector status over time, sector load against the split and merge thresholds, handoffs (completed / aborted / deferred), conflicts (detected / resolved / lost), spoof detections, detector run time |

Two sources feed Prometheus (`deploy/prometheus.yml`, scraped every 2 s):
every Raft node's `/metrics` (role, term, log indexes, tracks delivered and
dropped) and the gateway's `/metrics` (`cmd/gateway/metrics.go`), which
exports what only the gateway sees: sector load, handoff and clearance
counts and latency histograms, the sector map, conflicts, spoof detection and
the safety invariants.

Things worth watching during a demo:

- `scripts/chaos.sh kill-leader sector-1`: on the Raft dashboard one node row
  turns from leader to follower, a candidate appears, a new leader is
  elected, and "Leaders per cluster" dips to 0 and recovers. A clearance issued
  during the election waits in the gateway buffer and is counted under
  `path=buffered` in the clearance latency graph on the overview.
- `docker compose -f docker-compose.yml -f docker-compose.surge.yml --profile metrics up --build`:
  a sector climbs past the split threshold, the standby cluster turns ACTIVE,
  and later it is merged back (purple markers on the Overview and Elastic graphs mark each change).

Notes: the gateway's counters live in memory and start from 0 when the
gateway restarts (`rate()` and `increase()` handle that). Node metrics carry
`node` and `cluster` labels; gateway metrics carry `sector` or `cluster`.
The dashboards are plain JSON in `deploy/grafana/dashboards/`; edit them in
the Grafana UI, export the JSON and overwrite the file to keep a change.

## Experiments and graphs

```bash
docker compose build
python3 scripts/experiments/run.py all       # about 40 minutes; or one of:
                                             # failover scaling surge ghost chaos transport conflict
pip install matplotlib
python3 scripts/experiments/plot.py --summary   # results/figures/*.png + summary.json
```

Each experiment starts the stack itself, saves raw JSON to `results/`, and
ends with `docker compose down -v`. See the docstring at the top of
`scripts/experiments/run.py` for what each one measures.

## Compare the mutual exclusion approaches

```bash
docker compose --profile maekawa up -d --build
python3 scripts/compare.py --seconds 80 --crash-after 40
```

Crashes one Maekawa voter and the registry leader halfway, prints messages per
entry and blocking for both, and saves JSON to `results/`. See
[docs/how-it-works.md](docs/how-it-works.md) for what the numbers mean.

## gRPC

Every internal link uses gRPC (`proto/atc.proto`, port 9090): the gateway
calls each Raft node (service `Node`), and the simulator and ground receivers
report to the gateway (service `Gateway`). The radar still uses HTTP and the
WebSocket, since browsers cannot speak plain gRPC. The original HTTP/JSON
endpoints still exist, so the transport can be switched for comparison:

```bash
docker compose up --build                            # TRANSPORT=grpc (default)
TRANSPORT=http MTLS=off docker compose up --build    # original HTTP/JSON
```

## Mutual TLS

Every gRPC link uses mutual TLS. `docker compose up` first runs a one-shot
`certs` service (`scripts/gen-certs.sh`) that makes a dev CA and one
certificate per service in `./certs` (git-ignored; delete it for a new CA).
Each server checks the caller's certificate and then who it is: only the
gateway may write to a node, only the simulator may report positions, and a
receiver may only report under its own name. The HTTP write endpoints are
closed while mTLS is on.

```bash
docker compose run --rm intruder           # rogue container: 4 attempts to write to the registry, all refused
MTLS=off docker compose up -d              # before: no mTLS
MTLS=off docker compose run --rm intruder  # the same attempts succeed
TRANSPORT=http MTLS=off docker compose up  # original HTTP/JSON everywhere (comparison only)
```

### Regenerating the gRPC code

The generated code is committed (Go in `internal/atcpb/`, Python in
`sim/atc_pb2*.py`), so the Docker builds need no `protoc`. After changing
`proto/atc.proto`:

```bash
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.2
protoc -I proto --go_out=. --go_opt=module=atc \
  --go-grpc_out=. --go-grpc_opt=module=atc proto/atc.proto
pip install grpcio-tools==1.85.3
python -m grpc_tools.protoc -I proto --python_out=sim --grpc_python_out=sim proto/atc.proto
```

## Layout

```
cmd/node/          Raft replica (hashicorp/raft + raft-boltdb), HTTP: /status /state /apply /metrics, gRPC: service Node
cmd/gateway/       gateway: leader tracking, position multicast, handoff coordinator, WebSocket feed,
                   safety invariants, conflict detection, Prometheus /metrics
cmd/maekawa/       Maekawa baseline (grid voting sets, INQUIRE/FAILED/YIELD)
cmd/intruder/      mTLS demo: a rogue container trying to write to the registry
cmd/hashpw/        makes a bcrypt hash for deploy/users.json
internal/fsm/      replicated state machine: ownership states + key/value
internal/mtls/     mutual TLS setup and per-RPC authorization by certificate name
internal/atcpb/    generated gRPC code (from proto/atc.proto)
docs/              how-it-works.md (every file, the course mapping, the Maekawa comparison), collins_design.md (security and interface design)
proto/atc.proto    gRPC contract; generated Go code in internal/atcpb/
sim/               sim.py (live / record / replay, spoof mode), receiver.py (ground station)
dashboard/         React + Vite radar
deploy/            go.Dockerfile, prometheus.yml, grafana/ (datasource, dashboard provider, dashboards)
scripts/           chaos.sh (fault injection), compare.py (Maekawa vs registry), gen-certs.sh (mTLS),
                   experiments/ (run.py and plot.py for the report's experiments)
results/           experiment output (created by the scripts, git-ignored)
```

## Tests

```bash
go vet ./...
go test ./...                       # FSM, handoff, elastic planner, auth, clearances, safety rules, conflicts, gRPC, mTLS
```

GitHub Actions (`.github/workflows/ci.yml`) runs the Go tests and a Docker
Compose smoke test on every pull request.

## Results

The experiment results used in the final report (run on November 21, 2026)
are summarized below. `scripts/experiments/run.py` writes the raw JSON to
`results/`, which is git-ignored, so rerun the experiments to reproduce them.

| Experiment | Result |
|---|---|
| Leader failover | 18 of 18 crashes recovered, new leader in 2.65 s on average; every clearance sent during an election committed |
| Scaling (50 to 400 aircraft) | 39 to 95 ms per handoff, none aborted, gateway CPU under 5% |
| Surge | sector split 2.9 s after the overload, merged back at 86.9 s, 134 handoffs, none aborted |
| Ghost aircraft | 5 of 5 detected within 2.1 s, 0 of 100 real aircraft flagged |
| Chaos (10 faults) | all 6 safety rules held; 216 clearances committed, none lost |
| Transport | gRPC + mTLS about the same latency as HTTP/JSON (p50 about 29 ms) |
| Conflict detection | alerts about 119 s ahead; no loss of separation when the suggestion is issued |

## Known limitations

- The gateway is a single container, so it is a single point of failure.
  Handoffs in progress are stored in the registry and resume when it restarts.
- Leader election takes about 2.5 s with hashicorp/raft's default timeouts.
- After an elastic split, only the supervisor can clear aircraft in the standby half.
- The radar and login use plain HTTP; Raft traffic between replicas is outside mutual TLS.
- Ghost detection compares ranges from simulated receivers; it is a simplified
  form of multilateration, not real radio timing.
- There is one standby cluster, so the airspace grows to at most five sectors.
