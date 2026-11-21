
"""Aircraft simulator with JSON scenarios, recording, and replay."""

import json
import math
import os
import random
import time
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

from gateway_client import GatewayClient
from recording import record_frame, replay_frames

SIZE = 200.0  # Airspace width and height in nautical miles

AIRCRAFT = int(os.environ.get("AIRCRAFT", "100"))
TICK = float(os.environ.get("TICK", "1.0"))
SEED = os.environ.get("SEED")
SCENARIO = os.environ.get("SCENARIO", "").strip()

# Simulation modes: live, record, replay
MODE = os.environ.get("MODE", "live").strip().lower()
RECORD_FILE = os.environ.get("RECORD_FILE", "recordings/traffic.jsonl")
REPLAY_FILE = os.environ.get("REPLAY_FILE", "recordings/traffic.jsonl")

# Ground receivers that get each tick's transmitter positions (comma-separated
# base URLs). Empty: no receivers, the gateway's verification stays off.
RECEIVERS = [u.strip().rstrip("/") for u in os.environ.get("RECEIVERS", "").split(",") if u.strip()]

# Spoofing: SPOOF ghost aircraft on fake tracks, all transmitted from one
# ground station at (SPOOF_X, SPOOF_Y). Only for the random fleet; scenario
# files mark ghosts with an "emitter" field instead.
SPOOF = int(os.environ.get("SPOOF", "0"))
SPOOF_X = float(os.environ.get("SPOOF_X", "100"))
SPOOF_Y = float(os.environ.get("SPOOF_Y", "20"))

AIRCRAFT_FIELDS = ("callsign", "x", "y", "alt", "heading", "speed")

# Following clearances (live and record modes). The gateway's reply to every
# position report carries each aircraft's latest committed clearance; the
# aircraft then flies towards it at these rates.
FOLLOW = os.environ.get("FOLLOW_CLEARANCES", "on").strip().lower() not in ("off", "0", "false", "no")
CLIMB_FPS = 2000 / 60   # 2,000 ft/min climb or descent
TURN_DPS = 3.0          # standard-rate turn, degrees per second
ACCEL_KTPS = 2.0        # speed change, knots per second

# callsign -> {"applied": set of clearance ids, "alt"/"heading"/"speed": target}.
# Kept apart from the aircraft dicts so recordings and reports stay unchanged.
CONTROL = {}


def resolve_path(filename):
    """Resolve relative files inside the simulator's /app directory."""
    path = Path(filename)
    return path if path.is_absolute() else Path(__file__).resolve().parent / path


def make_aircraft(i, rng):
    return {
        "callsign": f"ATC{i:03d}",
        "x": rng.uniform(0, SIZE),
        "y": rng.uniform(0, SIZE),
        "alt": rng.choice(range(5000, 39001, 1000)),
        "heading": rng.uniform(0, 360),
        "speed": rng.uniform(250, 480),
    }


def make_ghost(i, rng):
    """A spoofed aircraft: claims a normal flight, transmits from the spoofer."""
    ghost = make_aircraft(i, rng)
    ghost["callsign"] = f"GHOST{i:02d}"
    ghost["emitter"] = {"x": SPOOF_X, "y": SPOOF_Y}
    return ghost


def make_fleet(rng):
    fleet = [make_aircraft(i, rng) for i in range(AIRCRAFT)]
    fleet += [make_ghost(i, rng) for i in range(1, SPOOF + 1)]
    return fleet


def emitter_of(ac):
    """Where the transmitter physically is (ground truth, never sent to the gateway)."""
    e = ac.get("emitter")
    if isinstance(e, dict):
        return e["x"], e["y"]
    return ac["x"], ac["y"]


def load_scenario(filename):
    """Load and validate aircraft from a JSON scenario file."""
    path = resolve_path(filename)

    with path.open("r", encoding="utf-8") as file:
        scenario = json.load(file)

    if not isinstance(scenario, dict) or not isinstance(scenario.get("aircraft"), list):
        raise ValueError("Scenario JSON must contain an 'aircraft' list")
    if not scenario["aircraft"]:
        raise ValueError("Scenario must contain at least one aircraft")

    fleet = []
    callsigns = set()

    for i, aircraft in enumerate(scenario["aircraft"], start=1):
        if not isinstance(aircraft, dict) or any(field not in aircraft for field in AIRCRAFT_FIELDS):
            raise ValueError(f"Aircraft {i} is missing required fields")

        callsign = aircraft["callsign"]
        if not isinstance(callsign, str) or not callsign.strip():
            raise ValueError(f"Aircraft {i} needs a valid callsign")
        if callsign in callsigns:
            raise ValueError(f"Duplicate callsign: {callsign}")
        callsigns.add(callsign)

        for field in AIRCRAFT_FIELDS[1:]:
            value = aircraft[field]
            if isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(value):
                raise ValueError(f"{callsign}: '{field}' must be a finite number")

        if not (0 <= aircraft["x"] < SIZE and 0 <= aircraft["y"] < SIZE):
            raise ValueError(f"{callsign}: x and y must be within the airspace")
        if aircraft["alt"] <= 0 or aircraft["speed"] < 0:
            raise ValueError(f"{callsign}: invalid altitude or speed")
        if not 0 <= aircraft["heading"] < 360:
            raise ValueError(f"{callsign}: heading must be from 0 to below 360")

        entry = {field: aircraft[field] for field in AIRCRAFT_FIELDS}

        # Optional: a ghost transmitted from a fixed spoofer position.
        emitter = aircraft.get("emitter")
        if emitter is not None:
            if not isinstance(emitter, dict) or any(
                isinstance(emitter.get(k), bool) or not isinstance(emitter.get(k), (int, float))
                or not math.isfinite(emitter[k]) for k in ("x", "y")
            ):
                raise ValueError(f"{callsign}: 'emitter' must be {{\"x\": number, \"y\": number}}")
            entry["emitter"] = {"x": emitter["x"], "y": emitter["y"]}

        fleet.append(entry)

    print(f"loaded scenario '{scenario.get('name', path.name)}' with {len(fleet)} aircraft", flush=True)
    return fleet


def parse_instruction(text):
    """Turn a clearance into targets: {"alt": ft} / {"heading": deg} / {"speed": kt}.

    Understands CLIMB/DESCEND [AND MAINTAIN] FLnnn, MAINTAIN FLnnn,
    [TURN LEFT|RIGHT] HEADING ddd, and [REDUCE|INCREASE] SPEED nnn.
    Anything else returns {} and the aircraft carries on.
    """
    words = text.upper().split()
    for i, w in enumerate(words):
        if w.startswith("FL") and w[2:].isdigit():
            return {"alt": int(w[2:]) * 100}
        if w == "HEADING" and i + 1 < len(words) and words[i + 1].isdigit():
            return {"heading": int(words[i + 1]) % 360}
        if w == "SPEED" and i + 1 < len(words) and words[i + 1].isdigit():
            return {"speed": int(words[i + 1])}
    return {}


def apply_instructions(fleet, instructions):
    """Take each committed clearance once (by clearance id), oldest first."""
    if not FOLLOW:
        return
    by_cs = {ac["callsign"]: ac for ac in fleet}
    for ins in instructions or []:
        cs = ins.get("callsign")
        if cs not in by_cs:
            continue
        ctl = CONTROL.setdefault(cs, {"applied": set()})
        if ins.get("clearance_id") in ctl["applied"]:
            continue
        ctl["applied"].add(ins.get("clearance_id"))
        targets = parse_instruction(ins.get("instruction", ""))
        ctl.update(targets)
        print(f"{cs}: flying clearance {ins.get('instruction')!r}" + ("" if targets else " (not understood, ignored)"), flush=True)


def _toward(value, target, rate):
    return target if abs(target - value) <= rate else value + math.copysign(rate, target - value)


def fly_targets(ac, dt):
    """Move altitude, heading and speed towards the cleared targets."""
    ctl = CONTROL.get(ac["callsign"])
    if not ctl:
        return
    if "alt" in ctl:
        ac["alt"] = _toward(ac["alt"], ctl["alt"], CLIMB_FPS * dt)
    if "speed" in ctl:
        ac["speed"] = _toward(ac["speed"], ctl["speed"], ACCEL_KTPS * dt)
    if "heading" in ctl:
        diff = (ctl["heading"] - ac["heading"] + 540) % 360 - 180  # shortest way round, -180..180
        ac["heading"] = (ac["heading"] + _toward(0, diff, TURN_DPS * dt)) % 360


def step(ac, dt):
    """Advance aircraft and increase its FIFO sequence number."""
    fly_targets(ac, dt)
    ac["seq"] = ac.get("seq", 0) + 1
    nm = ac["speed"] * dt / 3600.0
    rad = math.radians(ac["heading"])
    ac["x"] = (ac["x"] + nm * math.sin(rad)) % SIZE
    ac["y"] = (ac["y"] + nm * math.cos(rad)) % SIZE


gateway = GatewayClient("simulator")


def send(fleet):
    # The gateway only gets what the aircraft claims, not where it transmits from.
    claims = [{k: v for k, v in ac.items() if k != "emitter"} for ac in fleet]
    return gateway.positions(claims)


_pool = ThreadPoolExecutor(max_workers=max(1, 2 * len(RECEIVERS)))
_down = set()


def _broadcast_to(url, body):
    req = urllib.request.Request(url + "/broadcast", data=body, headers={"Content-Type": "application/json"})
    try:
        urllib.request.urlopen(req, timeout=0.5).read()
        if url in _down:
            _down.discard(url)
            print(f"receiver {url} reachable again", flush=True)
    except (urllib.error.URLError, OSError) as error:
        if url not in _down:  # log once per outage, not once per tick
            _down.add(url)
            print(f"receiver {url} unreachable: {error}", flush=True)


def broadcast(fleet):
    """Radio side of the tick: every receiver gets every transmitter's position.

    Sent in the background so a dead receiver never slows the simulator.
    """
    if not RECEIVERS:
        return
    rows = []
    for ac in fleet:
        if "seq" not in ac:
            continue
        ex, ey = emitter_of(ac)
        rows.append({"callsign": ac["callsign"], "seq": ac["seq"], "emitter_x": ex, "emitter_y": ey})
    body = json.dumps(rows).encode()
    for url in RECEIVERS:
        _pool.submit(_broadcast_to, url, body)


def send_safely(fleet, follow=False):
    try:
        instructions = send(fleet)
        if follow:
            apply_instructions(fleet, instructions)
    except (urllib.error.URLError, OSError) as error:
        print("gateway unreachable:", error, flush=True)
    broadcast(fleet)


def run_live():
    """Generate positions and optionally record every generated frame."""
    rng = random.Random(SEED)

    if SCENARIO:
        try:
            fleet = load_scenario(SCENARIO)
        except (OSError, ValueError) as error:
            raise SystemExit(f"Unable to load scenario: {error}") from error
    else:
        fleet = make_fleet(rng)

    ghosts = sum(1 for ac in fleet if "emitter" in ac)
    print(f"simulating {len(fleet)} aircraft ({ghosts} spoofed), tick {TICK}s -> {gateway.describe()}", flush=True)
    if RECEIVERS:
        print(f"broadcasting to receivers: {', '.join(RECEIVERS)}", flush=True)

    recorder = None

    try:
        if MODE == "record":
            path = resolve_path(RECORD_FILE)
            path.parent.mkdir(parents=True, exist_ok=True)
            recorder = path.open("w", encoding="utf-8")
            print(f"recording aircraft frames to {path}", flush=True)

        tick_number = 0

        while True:
            start = time.monotonic()

            for ac in fleet:
                step(ac, TICK)

            tick_number += 1

            if recorder is not None:
                record_frame(recorder, tick_number, fleet)

            send_safely(fleet, follow=True)

            time.sleep(max(0.0, TICK - (time.monotonic() - start)))

    finally:
        if recorder is not None:
            recorder.close()


def run_replay():
    """Send recorded frames without recalculating aircraft movement."""
    path = resolve_path(REPLAY_FILE)
    print(f"replaying aircraft frames from {path}", flush=True)

    frame_count = 0

    try:
        for frame in replay_frames(path):
            start = time.monotonic()

            send_safely(frame["aircraft"])
            frame_count += 1

            time.sleep(max(0.0, TICK - (time.monotonic() - start)))

    except (OSError, ValueError) as error:
        raise SystemExit(f"Unable to replay recording: {error}") from error

    print(f"replay complete: {frame_count} frames sent", flush=True)


def main():
    if TICK <= 0 or not math.isfinite(TICK):
        raise SystemExit("TICK must be a positive, finite number")

    if MODE not in ("live", "record", "replay"):
        raise SystemExit("MODE must be live, record, or replay")

    if MODE == "replay":
        run_replay()
    else:
        run_live()


if __name__ == "__main__":
    main()
