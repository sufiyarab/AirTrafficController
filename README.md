# Air Traffic Controller but Different

## What you need

- **Docker Desktop** (or Docker Engine + Compose v2). That is all you need to
  build and run everything. The Go compiler, Node and Python all run inside
  the build containers (`golang:1.26-alpine`, `node:22-alpine`, `python:3.12-slim`).
- Optional, for editing with autocomplete and running `go test` / `go vet`
  outside Docker: Go 1.26+. Optional for dashboard hot reload: Node 22.

## Run it

```bash
docker compose up --build            # first build takes a few minutes
docker compose --profile maekawa up --build   # also start the Maekawa baseline
```

- Radar dashboard: http://localhost:3000
- Gateway state (JSON): http://localhost:8080/api/state
- With metrics: `docker compose --profile metrics up --build`, then
  Prometheus on http://localhost:9090 and Grafana on http://localhost:3001

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

The simulator now flies committed clearances: the reply to each position report
(`ReportPositions` in `proto/atc.proto` now returns `Instructions`) carries the
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
| `FOLLOW_CLEARANCES` (simulator) | 1 | 0 = aircraft ignore clearances, as before |

Both can be set on the command line, e.g. `FOLLOW_CLEARANCES=0 docker compose up`.

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
                   safety invariants, conflict detection
cmd/maekawa/       Maekawa baseline (grid voting sets, INQUIRE/FAILED/YIELD)
cmd/intruder/      mTLS demo: a rogue container trying to write to the registry
internal/fsm/      replicated state machine: ownership states + key/value
internal/mtls/     mutual TLS setup and per-RPC authorization by certificate name
internal/atcpb/    generated gRPC code (from proto/atc.proto)
docs/              how-it-works.md: every file, the course mapping, the comparison
proto/atc.proto    gRPC contract; generated Go code in internal/atcpb/
sim/               sim.py (live / record / replay, spoof mode), receiver.py (ground station)
dashboard/         React + Vite radar
deploy/            go.Dockerfile, prometheus.yml, grafana datasource
scripts/          chaos.sh (fault injection), compare.py (Maekawa vs registry), gen-certs.sh (mTLS)
```

## Current state and next steps

Working now: all clusters elect leaders and replicate;
clearances need a controller login and are limited to the controller's sectors;
ground receivers cross-check every aircraft's claimed position and the radar
flags spoofed aircraft, which cannot receive clearances; positions reach the
controlling sector's replicas by B-multicast with FIFO sequence numbers;
handoffs run in 3 phases through the registry (begin, target accepts, commit),
abort when the target has no majority, and resume after a crash; the Maekawa
baseline and comparison script run; the radar shows aircraft (amber while
transferring), sectors, every node's role/term, and handoff counters; every
internal link is gRPC with mutual TLS, and only the gateway can write to a
Raft log.

Known limitation: after an elastic split, only the supervisor can issue
clearances in the half on the standby cluster (see docs/collins_design.md,
section 5).

- Elastic split/merge using the standby cluster, sector map in the registry (Issaq)
- Scenario files, conflict/stress modes, replay (Mashal)
- Conflict detection with suggested resolutions; the simulator follows clearances
- Grafana dashboards (Chimidike)
