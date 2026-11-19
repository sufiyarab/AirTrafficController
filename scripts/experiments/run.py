#!/usr/bin/env python3
"""Experiments for the final report. Each one starts the stack the way it
needs, measures, and saves raw JSON to results/<name>-<time>.json. Graphs are
made from those files by plot.py.

Run from the repo root, with the images already built (docker compose build):

    python3 scripts/experiments/run.py failover   # leader crash: election + catch-up time
    python3 scripts/experiments/run.py scaling    # 50..400 aircraft: handoff latency, CPU
    python3 scripts/experiments/run.py surge      # elastic split and merge timeline
    python3 scripts/experiments/run.py ghost      # spoof detection time, receiver loss
    python3 scripts/experiments/run.py chaos      # safety invariants under mixed faults
    python3 scripts/experiments/run.py transport  # HTTP/JSON vs gRPC vs gRPC + mTLS
    python3 scripts/experiments/run.py all        # all six, about 30 minutes

Only the Python standard library and the docker CLI are needed. Every
experiment ends with `docker compose down -v`.

Measurement notes for the report:
  - Times are taken from this script's view of the gateway's /api/state,
    polled every 100 ms. The gateway itself polls nodes every 500 ms, so an
    election time here is accurate to about +-0.5 s.
  - Handoff latency is the gateway's own measurement (begin -> accept ->
    commit, 3 Raft commits). The gateway reports a running mean, so the
    script turns it into the mean of each sampling interval.
"""
import argparse
import json
import os
import random
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request

GATEWAY = os.environ.get("GATEWAY", "http://localhost:8080")
ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
RESULTS = os.path.join(ROOT, "results")
NET = os.environ.get("NET", "atc_atc")
SUPERVISOR = ("supervisor", os.environ.get("SUPERVISOR_PASSWORD", "supervisor-demo"))
SECTORS = ["sector-1", "sector-2", "sector-3", "sector-4", "tower"]


# ---------- plumbing ----------

def log(*a):
    print(time.strftime("%H:%M:%S"), *a, flush=True)


def compose(*args, files=(), env=None, check=True):
    cmd = ["docker", "compose"]
    for f in files:
        cmd += ["-f", f]
    cmd += list(args)
    e = dict(os.environ)
    e.update(env or {})
    return subprocess.run(cmd, cwd=ROOT, env=e, capture_output=True, text=True, check=check).stdout


def http(method, path, body=None, token=None, timeout=3):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(GATEWAY + path, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, json.loads(r.read() or b"null")
    except urllib.error.HTTPError as e:
        try:
            return e.code, json.loads(e.read() or b"null")
        except ValueError:
            return e.code, None


def state():
    try:
        code, s = http("GET", "/api/state")
        return s if code == 200 else None
    except (urllib.error.URLError, OSError, ValueError):
        return None


def login():
    code, r = http("POST", "/api/login", {"username": SUPERVISOR[0], "password": SUPERVISOR[1]})
    return r.get("token") if code == 200 and r else None


def healthy(s, want_aircraft=1):
    if not s:
        return False
    ready = s.get("ready") or {}
    return len(ready) >= 7 and all(r.get("ready") for r in ready.values()) and len(s.get("aircraft") or []) >= want_aircraft


def up(files=(), env=None, want_aircraft=1, timeout=180, sim_later=False):
    """Start the stack and wait until every cluster is Ready. With sim_later,
    the simulator is held back until the clusters are Ready and started by
    the caller (start_sim), so the very first aircraft reports are measured."""
    compose("down", "-v", files=files, check=False)
    log("starting stack", " ".join(os.path.basename(f) for f in files) or "(default)", env or "")
    if sim_later:
        compose("up", "-d", "--scale", "simulator=0", files=files, env=env)
        want_aircraft = 0
    else:
        compose("up", "-d", files=files, env=env)
    t0 = time.time()
    while time.time() - t0 < timeout:
        if healthy(state(), want_aircraft):
            log(f"stack healthy after {time.time() - t0:.0f}s")
            return
        time.sleep(2)
    raise SystemExit("stack did not become healthy; see docker compose logs")


def start_sim(files=(), env=None):
    compose("up", "-d", "simulator", files=files, env=env)
    log("simulator started")
    return time.time()


def down(files=()):
    compose("down", "-v", files=files, check=False)


def override(name, text):
    """Write a temporary compose override file and return its path."""
    os.makedirs(RESULTS, exist_ok=True)
    path = os.path.join(RESULTS, f".override-{name}.yml")
    with open(path, "w") as f:
        f.write(text)
    return path


def save(name, data):
    os.makedirs(RESULTS, exist_ok=True)
    path = os.path.join(RESULTS, f"{name}-{time.strftime('%Y%m%d-%H%M%S')}.json")
    with open(path, "w") as f:
        json.dump(data, f, indent=1)
    log("saved", os.path.relpath(path, ROOT))
    return path


def safety_summary():
    code, s = http("GET", "/api/safety")
    if code != 200 or not s:
        return None
    return {"ok": s["ok"], "invariants": {i["id"]: {"ok": i["ok"], "checks": i["checks"], "violations": i["violations"],
                                                    "last_violation": i.get("last_violation")} for i in s["invariants"]},
            "recent": s.get("recent") or []}


def leader(s, cluster):
    for n in s["clusters"][cluster]:
        if n.get("up") and n.get("state") == "Leader":
            return n
    return None


def owned_by(s, cluster):
    return [a["callsign"] for a in (s.get("aircraft") or []) if a.get("owner") == cluster and a.get("status") == "ACTIVE"
            and a.get("verify") != "SUSPECT"]


def wait_clearance(cid, t0, timeout=20):
    """Poll the dashboard feed until clearance cid is no longer pending.
    Returns its final status and the seconds since t0 (when it was sent)."""
    while time.time() - t0 < timeout:
        s = state()
        for c in (s or {}).get("clearances") or []:
            if c["id"] == cid and c["status"] != "pending":
                return c["status"], time.time() - t0
        time.sleep(0.1)
    return "pending", time.time() - t0


# ---------- 1. failover ----------

def failover(args):
    """Kill the leader of each cluster `rounds` times. Measure the time until
    the gateway sees a new single leader (election), the fate of a clearance
    sent right after the crash, and how long the restarted node takes to catch
    up with the leader's commit index."""
    up()
    token = login()
    clusters = SECTORS + ["registry"]
    rows = []
    for rnd in range(args.rounds):
        for c in clusters:
            s = state()
            old = leader(s, c)
            if not old:
                log(c, "has no leader, skipping")
                continue
            victim = old["id"]
            target = (owned_by(s, c) or [None])[0] if c != "registry" else None
            t_kill = time.time()
            compose("kill", victim)
            clr = None
            if target and token:
                t_sent = time.time()
                code, r = http("POST", "/api/clearance", {"callsign": target, "instruction": "MAINTAIN FL300"}, token)
                clr = {"http": code, "status": (r or {}).get("status"), "id": (r or {}).get("id")}
            # election
            t_elect, new = None, None
            while time.time() - t_kill < 20:
                s = state()
                r = (s or {}).get("ready", {}).get(c, {})
                if s and r.get("ready") and r.get("leader") and r.get("leader") != victim:
                    t_elect, new = time.time() - t_kill, r["leader"]
                    break
                time.sleep(0.1)
            if clr and clr["status"] == "pending":
                clr["final"], clr["wait_s"] = wait_clearance(clr["id"], t_sent)
            elif clr:
                clr["final"], clr["wait_s"] = clr["status"], 0
            # recovery: restart the old leader and wait until it has caught up
            time.sleep(1)
            s = state()
            target_commit = int((leader(s, c) or {}).get("commit_index") or 0)
            t_start = time.time()
            compose("start", victim)
            t_catch = None
            while time.time() - t_start < 30:
                s = state()
                n = next((n for n in (s or {}).get("clusters", {}).get(c, []) if n["id"] == victim), None)
                if n and n.get("up") and int(n.get("commit_index") or 0) >= target_commit:
                    t_catch = time.time() - t_start
                    break
                time.sleep(0.1)
            row = {"round": rnd, "cluster": c, "killed": victim, "new_leader": new,
                   "election_s": t_elect, "catchup_s": t_catch, "catchup_to_index": target_commit, "clearance": clr}
            log(f"{c}: killed {victim}, new leader {new} after {t_elect and round(t_elect, 2)}s, "
                f"restarted node caught up in {t_catch and round(t_catch, 2)}s, clearance {clr and clr.get('final')}")
            rows.append(row)
            time.sleep(3)
    out = {"rows": rows, "safety": safety_summary(), "handoff": state()["handoff"]}
    down()
    return save("failover", out)


# ---------- 2. scaling ----------

def docker_stats(services):
    ids = {}
    for svc in services:
        cid = compose("ps", "-q", svc, check=False).strip()
        if cid:
            ids[cid[:12]] = svc
    if not ids:
        return {}
    out = subprocess.run(["docker", "stats", "--no-stream", "--format", "{{.ID}} {{.CPUPerc}} {{.MemUsage}}"] + list(ids),
                         capture_output=True, text=True).stdout
    res = {}
    for line in out.splitlines():
        parts = line.split()
        if len(parts) >= 2 and parts[0][:12] in ids:
            res[ids[parts[0][:12]]] = {"cpu_pct": float(parts[1].rstrip("%") or 0), "mem": parts[2] if len(parts) > 2 else ""}
    return res


def scaling(args):
    """Run the stack at several fleet sizes and measure handoff rate and
    latency, multicast volume, and CPU of the gateway and a sector leader."""
    runs = []
    for n in args.sizes:
        f = override(f"scale{n}", f"services:\n  simulator:\n    environment:\n      AIRCRAFT: \"{n}\"\n")
        files = ("docker-compose.yml", f)
        up(files, want_aircraft=n)
        time.sleep(args.warmup)
        samples, prev = [], state()["handoff"]
        t_end = time.time() + args.seconds
        while time.time() < t_end:
            time.sleep(2)
            s = state()
            h = s["handoff"]
            dn = h["handoffs"] - prev["handoffs"]
            mean = (h["avg_ms"] * h["handoffs"] - prev["avg_ms"] * prev["handoffs"]) / dn if dn > 0 else None
            lead = leader(s, "sector-1")
            cpu = docker_stats(["gateway"] + ([lead["id"]] if lead else []))
            samples.append({"t": time.time(), "handoffs": dn, "interval_mean_ms": mean,
                            "multicast": h["multicast_batches"] - prev["multicast_batches"],
                            "dropped": h["multicast_dropped"] - prev["multicast_dropped"],
                            "aborted": h["aborted"] - prev["aborted"], "aircraft": len(s.get("aircraft") or []),
                            "gateway_cpu": cpu.get("gateway", {}).get("cpu_pct"),
                            "leader_cpu": cpu.get(lead["id"], {}).get("cpu_pct") if lead else None})
            prev = h
        total = sum(x["handoffs"] for x in samples)
        means = [x["interval_mean_ms"] for x in samples if x["interval_mean_ms"] is not None]
        wsum = sum(x["interval_mean_ms"] * x["handoffs"] for x in samples if x["interval_mean_ms"] is not None)
        run = {"aircraft": n, "seconds": args.seconds, "samples": samples,
               "handoffs_per_min": total * 60 / args.seconds,
               "mean_handoff_ms": wsum / total if total else None,
               "max_interval_mean_ms": max(means) if means else None,
               "multicast_per_s": sum(x["multicast"] for x in samples) / args.seconds,
               "aborted": sum(x["aborted"] for x in samples),
               "gateway_cpu_avg": avg([x["gateway_cpu"] for x in samples]),
               "leader_cpu_avg": avg([x["leader_cpu"] for x in samples]),
               "safety": safety_summary()}
        log(f"{n} aircraft: {run['handoffs_per_min']:.0f} handoffs/min, mean {fmt(run['mean_handoff_ms'])} ms, "
            f"gateway CPU {fmt(run['gateway_cpu_avg'])}%")
        runs.append(run)
        down(files)
    return save("scaling", {"runs": runs})


def avg(xs):
    xs = [x for x in xs if x is not None]
    return sum(xs) / len(xs) if xs else None


def fmt(x):
    return "-" if x is None else f"{x:.1f}"


# ---------- 3. surge ----------

def surge(args):
    """The surge scenario: 60 aircraft start in sector-1 and spread out.
    Sample every second: each sector's load and status, and the controller's
    split / merge events."""
    files = ("docker-compose.yml", "docker-compose.surge.yml")
    up(files, sim_later=True)
    t0, samples, events = start_sim(files), [], {}
    while time.time() - t0 < args.seconds:
        s = state()
        if s:
            el = s["elastic"]
            samples.append({"t": time.time() - t0, "version": el.get("version"),
                            "sectors": {x["name"]: {"status": x["status"], "load": x["load"]} for x in el["sectors"]},
                            "handoffs": s["handoff"]["handoffs"], "aborted": s["handoff"]["aborted"],
                            "safety_ok": (s.get("safety") or {}).get("ok")})
            for e in el.get("events") or []:
                events[(e["time_ms"], e["kind"], e["sector"])] = {**e, "t": e["time_ms"] / 1000 - t0}
        time.sleep(1)
    out = {"samples": samples, "events": sorted(events.values(), key=lambda e: e["t"]),
           "config": {k: state()["elastic"].get(k) for k in ("split_at", "merge_at")}, "safety": safety_summary()}
    for e in out["events"]:
        log(f"t={e['t']:.1f}s {e['kind']} {e['sector']}: {e.get('detail', '')}")
    down(files)
    return save("surge", out)


# ---------- 4. ghost detection ----------

def ghost(args):
    """100 real aircraft plus SPOOF ghosts. Measure how long each ghost takes
    to turn SUSPECT and whether any real aircraft is ever flagged. Then stop
    one receiver and watch coverage drop, then bring it back."""
    env = {"SPOOF": str(args.ghosts)}
    up(env=env, sim_later=True)
    first_seen, suspect_at, false_flags, samples = {}, {}, set(), []
    t0 = start_sim(env=env)
    phases = [(args.seconds, "all receivers"), (args.seconds, "receiver-b stopped"), (args.seconds / 2, "receiver-b back")]
    for dur, phase in phases:
        if phase == "receiver-b stopped":
            compose("stop", "receiver-b")
        if phase == "receiver-b back":
            compose("start", "receiver-b")
        t_end = time.time() + dur
        while time.time() < t_end:
            s = state()
            if s:
                now = time.time() - t0
                counts = {"VERIFIED": 0, "UNVERIFIED": 0, "SUSPECT": 0}
                for a in (s.get("aircraft") or []):
                    cs, v = a["callsign"], a.get("verify")
                    counts[v] = counts.get(v, 0) + 1
                    first_seen.setdefault(cs, now)
                    if v == "SUSPECT":
                        if cs.startswith("GHOST"):
                            suspect_at.setdefault(cs, now)
                        else:
                            false_flags.add(cs)
                samples.append({"t": now, "phase": phase, **counts,
                                "receivers_up": sum(1 for r in s.get("receivers") or [] if r.get("up"))})
            time.sleep(0.25)
    ghosts = sorted(cs for cs in first_seen if cs.startswith("GHOST"))
    det = {cs: (suspect_at[cs] - first_seen[cs]) if cs in suspect_at else None for cs in ghosts}
    out = {"ghosts": len(ghosts), "detected": sum(1 for v in det.values() if v is not None), "detection_s": det,
           "false_positives": sorted(false_flags), "real_aircraft": len(first_seen) - len(ghosts),
           "samples": samples, "verify": state()["verify"], "safety": safety_summary()}
    log(f"ghosts detected {out['detected']}/{len(ghosts)}, times {[round(v, 2) for v in det.values() if v is not None]}, "
        f"false positives {len(false_flags)}")
    down()
    return save("ghost", out)


# ---------- 6. transport: HTTP/JSON vs gRPC vs gRPC + mTLS ----------

TRANSPORTS = [  # label, TRANSPORT, MTLS
    ("HTTP/JSON", "http", "off"),
    ("gRPC", "grpc", "off"),
    ("gRPC + mTLS", "grpc", "on"),
]


def pct(xs, q):
    xs = sorted(xs)
    if not xs:
        return None
    k = (len(xs) - 1) * q
    lo = int(k)
    return xs[lo] + (xs[min(lo + 1, len(xs) - 1)] - xs[lo]) * (k - lo)


def transport(args):
    """Same fleet, three transports between the gateway and the nodes (and the
    simulator/receivers and the gateway). Separates what gRPC changes from
    what TLS costs.

    Two latencies, both ending in a Raft commit over the transport under test:
      - clearance round trip, timed by this script: POST /api/clearance ->
        gateway reads the owner from the registry -> commits on the sector
        leader -> reply. Many samples (one every 200 ms).
      - handoff, timed by the gateway: begin -> accept -> commit, three Raft
        commits on two clusters. Fewer samples (depends on traffic).
    The script -> gateway hop is plain HTTP in all three runs, so it is the
    same constant in every column."""
    runs = []
    f = override("transport", f"services:\n  simulator:\n    environment:\n      AIRCRAFT: \"{args.aircraft}\"\n")
    files = ("docker-compose.yml", f)
    for label, tr, mtls in TRANSPORTS:
        env = {"TRANSPORT": tr, "MTLS": mtls}
        up(files, env=env, want_aircraft=args.aircraft // 2)
        time.sleep(args.warmup)
        token = login()
        clr, refused, samples = [], 0, []
        prev = state()["handoff"]
        t_end, next_sample = time.time() + args.seconds, time.time() + 2
        while time.time() < t_end:
            s = state()
            targets = [cs for c in SECTORS for cs in owned_by(s or {}, c)]
            if targets:
                t0 = time.time()
                code, r = http("POST", "/api/clearance", {"callsign": random.choice(targets), "instruction": "MAINTAIN FL300"}, token)
                ms = (time.time() - t0) * 1000
                if code == 200 and (r or {}).get("status") == "committed":
                    clr.append(ms)
                else:
                    refused += 1  # pending (no leader), 409 (handoff running), ...: not a transport timing
            time.sleep(0.2)
            if time.time() >= next_sample:
                next_sample += 2
                s = state()
                h = s["handoff"]
                dn = h["handoffs"] - prev["handoffs"]
                mean = (h["avg_ms"] * h["handoffs"] - prev["avg_ms"] * prev["handoffs"]) / dn if dn > 0 else None
                cpu = docker_stats(["gateway"])
                samples.append({"t": time.time(), "handoffs": dn, "interval_mean_ms": mean,
                                "multicast": h["multicast_batches"] - prev["multicast_batches"],
                                "dropped": h["multicast_dropped"] - prev["multicast_dropped"],
                                "gateway_cpu": cpu.get("gateway", {}).get("cpu_pct")})
                prev = h
        total = sum(x["handoffs"] for x in samples)
        wsum = sum(x["interval_mean_ms"] * x["handoffs"] for x in samples if x["interval_mean_ms"] is not None)
        run = {"label": label, "transport": tr, "mtls": mtls, "aircraft": args.aircraft, "seconds": args.seconds,
               "clearance_ms": clr, "clearances": len(clr), "clearances_not_committed": refused,
               "clearance_p50_ms": pct(clr, 0.5), "clearance_p95_ms": pct(clr, 0.95),
               "handoffs": total, "mean_handoff_ms": wsum / total if total else None,
               "multicast_per_s": sum(x["multicast"] for x in samples) / args.seconds,
               "multicast_dropped": sum(x["dropped"] for x in samples),
               "gateway_cpu_avg": avg([x["gateway_cpu"] for x in samples]),
               "samples": samples, "safety": safety_summary()}
        log(f"{label}: clearance p50 {fmt(run['clearance_p50_ms'])} ms, p95 {fmt(run['clearance_p95_ms'])} ms "
            f"({len(clr)} committed), handoff mean {fmt(run['mean_handoff_ms'])} ms ({total}), "
            f"gateway CPU {fmt(run['gateway_cpu_avg'])}%")
        runs.append(run)
        down(files)
    return save("transport", {"runs": runs})


# ---------- 5. safety invariants under mixed faults ----------

def chaos(args):
    """Issue a clearance every 0.5 s to a random aircraft while a sequence of
    faults runs: leader crashes in a sector and in the registry, a follower
    partition, a paused leader, and a manual split and merge. At the end, read
    the safety panel. Every invariant must have held."""
    up()
    token = login()
    stop = threading.Event()
    outcomes = []

    def traffic():
        while not stop.is_set():
            s = state()
            if s and s.get("aircraft"):
                a = random.choice(s["aircraft"])
                t = time.time()
                try:
                    code, r = http("POST", "/api/clearance", {"callsign": a["callsign"], "instruction": "HEADING 270"}, token)
                    outcomes.append({"t": t, "http": code, "status": (r or {}).get("status")})
                except (urllib.error.URLError, OSError):
                    outcomes.append({"t": t, "http": None, "status": "gateway unreachable"})
            time.sleep(0.5)

    th = threading.Thread(target=traffic, daemon=True)
    th.start()
    t0 = time.time()
    actions = []

    def act(what, fn):
        actions.append({"t": time.time() - t0, "action": what})
        log(what)
        fn()

    def lead(c):
        return leader(state(), c)["id"]

    time.sleep(10)
    v = lead("sector-1"); act(f"kill sector-1 leader {v}", lambda: compose("kill", v)); time.sleep(8)
    act(f"restart {v}", lambda: compose("start", v)); time.sleep(8)
    v2 = lead("registry"); act(f"kill registry leader {v2}", lambda: compose("kill", v2)); time.sleep(8)
    act(f"restart {v2}", lambda: compose("start", v2)); time.sleep(8)
    l2 = lead("sector-2")
    f2 = next(n["id"] for n in state()["clusters"]["sector-2"] if n["id"] != l2)
    cid = compose("ps", "-q", f2).strip()
    act(f"partition sector-2 follower {f2}", lambda: subprocess.run(["docker", "network", "disconnect", NET, cid], check=True))
    time.sleep(10)
    act(f"heal {f2}", lambda: subprocess.run(["docker", "network", "connect", "--alias", f2, NET, cid], check=True))
    time.sleep(8)
    v3 = lead("sector-3"); act(f"pause sector-3 leader {v3}", lambda: compose("pause", v3)); time.sleep(6)
    act(f"unpause {v3}", lambda: compose("unpause", v3)); time.sleep(8)
    act("manual split of sector-4", lambda: http("POST", "/api/elastic/split", {"sector": "sector-4"}, token)); time.sleep(20)
    act("manual merge back", lambda: http("POST", "/api/elastic/merge", {"sector": "standby"}, token)); time.sleep(20)
    stop.set()
    th.join()
    time.sleep(3)
    summary = {}
    for o in outcomes:
        summary[o["status"]] = summary.get(o["status"], 0) + 1
    out = {"actions": actions, "clearances": summary, "outcomes": [{**o, "t": o["t"] - t0} for o in outcomes],
           "safety": safety_summary(), "handoff": state()["handoff"], "clearance_stats": state()["clearance_stats"]}
    log("clearances (status in the first answer):", summary, "final:", out["clearance_stats"])
    log("safety:", {k: (v["ok"], v["checks"], v["violations"]) for k, v in out["safety"]["invariants"].items()})
    down()
    return save("chaos", out)


def main():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("experiment", choices=["failover", "scaling", "surge", "ghost", "chaos", "transport", "all"])
    p.add_argument("--rounds", type=int, default=3, help="failover: crashes per cluster")
    p.add_argument("--sizes", type=int, nargs="+", default=[50, 100, 200, 400], help="scaling: fleet sizes")
    p.add_argument("--seconds", type=int, default=None, help="measuring time per run")
    p.add_argument("--warmup", type=int, default=15, help="scaling: seconds before measuring")
    p.add_argument("--ghosts", type=int, default=5, help="ghost: number of spoofed aircraft")
    p.add_argument("--aircraft", type=int, default=100, help="transport: fleet size")
    args = p.parse_args()
    todo = ["failover", "scaling", "surge", "ghost", "chaos", "transport"] if args.experiment == "all" else [args.experiment]
    defaults = {"scaling": 60, "surge": 150, "ghost": 40, "transport": 90}
    sec = args.seconds
    for name in todo:
        args.seconds = sec or defaults.get(name, 60)
        globals()[name](args)


if __name__ == "__main__":
    main()
