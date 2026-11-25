
"""Aircraft simulator for the distributed Air Traffic Controller project.

Supports random aircraft, predefined JSON scenarios, recorded traffic, and replay.
The gateway handles coordination; this module generates and reports positions.
"""

import json
import math
import os
import random
import time
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

# GatewayClient supports the team's HTTP or gRPC communication layer.
from gateway_client import GatewayClient

# Recording helpers handle JSON Lines (one complete frame per line).
from recording import record_frame, replay_frames

SIZE = 200.0  # Airspace width and height in nautical miles

# Environment variables let Docker Compose control the simulator without editing code.
AIRCRAFT = int(os.environ.get("AIRCRAFT", "100"))
TICK = float(os.environ.get("TICK", "1.0"))
SEED = os.environ.get("SEED")
SCENARIO = os.environ.get("SCENARIO", "").strip()

# Simulation modes: live, record, replay
MODE = os.environ.get("MODE", "live").strip().lower()
RECORD_FILE = os.environ.get("RECORD_FILE", "recordings/traffic.jsonl")
REPLAY_FILE = os.environ.get("REPLAY_FILE", "recordings/traffic.jsonl")

# Ground receivers get each tick's transmitter positions.
# An empty list disables reporting to ground receivers.
RECEIVERS = [
    u.strip().rstrip("/")
    for u in os.environ.get("RECEIVERS", "").split(",")
    if u.strip()
]

# Ghost aircraft claim false positions but transmit from a fixed location.
# Scenarios can also specify an individual aircraft's "emitter" position.
SPOOF = int(os.environ.get("SPOOF", "0"))
SPOOF_X = float(os.environ.get("SPOOF_X", "100"))
SPOOF_Y = float(os.environ.get("SPOOF_Y", "20"))

AIRCRAFT_FIELDS = ("callsign", "x", "y", "alt", "heading", "speed")

# Follow committed controller clearances during live and record modes.
FOLLOW = os.environ.get("FOLLOW_CLEARANCES", "on").strip().lower() not in (
    "off", "0", "false", "no"
)

CLIMB_FPS = 2000 / 60  # Climb/descent rate: feet per second
TURN_DPS = 3.0         # Maximum turning rate: degrees per second
ACCEL_KTPS = 2.0       # Maximum speed change: knots per second

# Stores the last applied clearance IDs and aircraft movement targets.
# Kept separately so recorded aircraft data stays unchanged.
CONTROL = {}


def resolve_path(filename):
    """Resolve a file path relative to the simulator directory."""
    path = Path(filename)

    return (
        path
        if path.is_absolute()
        else Path(__file__).resolve().parent / path
    )


def make_aircraft(i, rng):
    """Create one aircraft with random starting properties."""
    return {
        "callsign": f"ATC{i:03d}",
        "x": rng.uniform(0, SIZE),
        "y": rng.uniform(0, SIZE),
        "alt": rng.choice(range(5000, 39001, 1000)),
        "heading": rng.uniform(0, 360),
        "speed": rng.uniform(250, 480),
    }


def make_ghost(i, rng):
    """Create an aircraft that reports a false transmitter location."""
    ghost = make_aircraft(i, rng)

    ghost["callsign"] = f"GHOST{i:02d}"
    ghost["emitter"] = {
        "x": SPOOF_X,
        "y": SPOOF_Y,
    }

    return ghost


def make_fleet(rng):
    """Generate normal aircraft and optional ghost aircraft."""
    fleet = [make_aircraft(i, rng) for i in range(AIRCRAFT)]

    fleet += [
        make_ghost(i, rng)
        for i in range(1, SPOOF + 1)
    ]

    return fleet


def emitter_of(ac):
    """Return the aircraft's actual transmitter position."""
    emitter = ac.get("emitter")

    if isinstance(emitter, dict):
        return emitter["x"], emitter["y"]

    return ac["x"], ac["y"]


def load_scenario(filename):
    """Load and validate predefined aircraft from a JSON file."""
    path = resolve_path(filename)

    # Read the scenario containing predefined aircraft.
    with path.open("r", encoding="utf-8") as file:
        scenario = json.load(file)

    # Ensure the JSON contains a valid aircraft list.
    if (
        not isinstance(scenario, dict)
        or not isinstance(scenario.get("aircraft"), list)
    ):
        raise ValueError(
            "Scenario JSON must contain an 'aircraft' list"
        )

    if not scenario["aircraft"]:
        raise ValueError(
            "Scenario must contain at least one aircraft"
        )

    fleet = []
    callsigns = set()

    for i, aircraft in enumerate(scenario["aircraft"], start=1):

        # Every aircraft requires the six standard properties.
        if (
            not isinstance(aircraft, dict)
            or any(
                field not in aircraft
                for field in AIRCRAFT_FIELDS
            )
        ):
            raise ValueError(
                f"Aircraft {i} is missing required fields"
            )

        callsign = aircraft["callsign"]

        # Callsigns must be valid and unique.
        if not isinstance(callsign, str) or not callsign.strip():
            raise ValueError(
                f"Aircraft {i} needs a valid callsign"
            )

        if callsign in callsigns:
            raise ValueError(
                f"Duplicate callsign: {callsign}"
            )

        callsigns.add(callsign)

        # All numerical properties must contain finite numbers.
        for field in AIRCRAFT_FIELDS[1:]:
            value = aircraft[field]

            if (
                isinstance(value, bool)
                or not isinstance(value, (int, float))
                or not math.isfinite(value)
            ):
                raise ValueError(
                    f"{callsign}: '{field}' must be a finite number"
                )

        # Keep the aircraft inside the simulated airspace.
        if not (
            0 <= aircraft["x"] < SIZE
            and 0 <= aircraft["y"] < SIZE
        ):
            raise ValueError(
                f"{callsign}: x and y must be within the airspace"
            )

        # Altitude must be positive, and speed cannot be negative.
        if aircraft["alt"] <= 0 or aircraft["speed"] < 0:
            raise ValueError(
                f"{callsign}: invalid altitude or speed"
            )

        # Heading is measured clockwise from north.
        if not 0 <= aircraft["heading"] < 360:
            raise ValueError(
                f"{callsign}: heading must be from 0 to below 360"
            )

        # Copy the validated aircraft properties.
        entry = {
            field: aircraft[field]
            for field in AIRCRAFT_FIELDS
        }

        # Optional transmitter position for a spoofed aircraft.
        emitter = aircraft.get("emitter")

        if emitter is not None:
            if (
                not isinstance(emitter, dict)
                or any(
                    isinstance(emitter.get(k), bool)
                    or not isinstance(emitter.get(k), (int, float))
                    or not math.isfinite(emitter[k])
                    for k in ("x", "y")
                )
            ):
                raise ValueError(
                    f'{callsign}: \'emitter\' must be '
                    '{"x": number, "y": number}'
                )

            entry["emitter"] = {
                "x": emitter["x"],
                "y": emitter["y"],
            }

        fleet.append(entry)

    print(
        f"loaded scenario "
        f"'{scenario.get('name', path.name)}' "
        f"with {len(fleet)} aircraft",
        flush=True,
    )

    return fleet


def parse_instruction(text):
    """Convert a controller clearance into aircraft movement targets.

    Supports flight levels, headings, turning directions, and speeds.
    """
    words = text.upper().split()

    for i, word in enumerate(words):

        # Example: CLIMB FL350 -> target altitude of 35,000 ft.
        if word.startswith("FL") and word[2:].isdigit():
            return {
                "alt": int(word[2:]) * 100
            }

        # Example: TURN LEFT HEADING 090.
        if (
            word == "HEADING"
            and i + 1 < len(words)
            and words[i + 1].isdigit()
        ):
            targets = {
                "heading": int(words[i + 1]) % 360
            }

            # FIX #2: Remember an explicitly requested turn direction.
            if i > 0 and words[i - 1] in ("LEFT", "RIGHT"):
                targets["turn"] = words[i - 1].lower()

            return targets

        # Example: REDUCE SPEED 250 -> target speed of 250 knots.
        if (
            word == "SPEED"
            and i + 1 < len(words)
            and words[i + 1].isdigit()
        ):
            return {
                "speed": int(words[i + 1])
            }

    # Unsupported instructions do not change the aircraft.
    return {}


def apply_instructions(fleet, instructions):
    """Apply each committed clearance only once."""
    if not FOLLOW:
        return

    # Find aircraft quickly using their callsigns.
    by_cs = {
        ac["callsign"]: ac
        for ac in fleet
    }

    for ins in instructions or []:
        callsign = ins.get("callsign")

        if callsign not in by_cs:
            continue

        # Store the aircraft's active targets and applied IDs.
        control = CONTROL.setdefault(
            callsign,
            {"applied": set()}
        )

        clearance_id = ins.get("clearance_id")

        # Avoid applying the same clearance repeatedly.
        if clearance_id in control["applied"]:
            continue

        control["applied"].add(clearance_id)

        targets = parse_instruction(
            ins.get("instruction", "")
        )

        # FIX #2: A plain heading instruction must not inherit
        # the LEFT or RIGHT direction of an older clearance.
        if "heading" in targets:
            control.pop("turn", None)

        control.update(targets)

        print(
            f"{callsign}: flying clearance "
            f"{ins.get('instruction')!r}"
            + (
                ""
                if targets
                else " (not understood, ignored)"
            ),
            flush=True,
        )


def _toward(value, target, rate):
    """Move toward a target without exceeding the allowed step."""
    if abs(target - value) <= rate:
        return target

    return (
        value
        + math.copysign(rate, target - value)
    )


def fly_targets(ac, dt):
    """Apply gradual altitude, speed, and heading changes."""
    control = CONTROL.get(ac["callsign"])

    if not control:
        return

    # Change altitude at the configured climb/descent rate.
    if "alt" in control:
        ac["alt"] = _toward(
            ac["alt"],
            control["alt"],
            CLIMB_FPS * dt,
        )

    # Change speed gradually instead of instantaneously.
    if "speed" in control:
        ac["speed"] = _toward(
            ac["speed"],
            control["speed"],
            ACCEL_KTPS * dt,
        )

    if "heading" in control:
        current = ac["heading"]
        target = control["heading"]

        # Maximum degrees the aircraft can turn this tick.
        max_turn = TURN_DPS * dt

        # FIX #2: Follow the explicitly requested LEFT turn.
        if control.get("turn") == "left":

            # Left turns decrease compass headings.
            remaining = (current - target) % 360

            change = -min(
                remaining,
                max_turn,
            )

        # FIX #2: Follow the explicitly requested RIGHT turn.
        elif control.get("turn") == "right":

            # Right turns increase compass headings.
            remaining = (target - current) % 360

            change = min(
                remaining,
                max_turn,
            )

        else:
            # No direction specified: choose the shortest turn.
            diff = (
                target - current + 540
            ) % 360 - 180

            change = _toward(
                0,
                diff,
                max_turn,
            )

        # Modulo handles crossing from 0 to 359 or 359 to 0.
        ac["heading"] = (
            current + change
        ) % 360


def step(ac, dt):
    """Advance one aircraft and increment its FIFO sequence number."""

    # First apply any active controller clearance.
    fly_targets(ac, dt)

    # Sequence numbers distinguish newer from older position updates.
    ac["seq"] = ac.get("seq", 0) + 1

    # Convert knots (NM/hour) into distance travelled this tick.
    distance_nm = ac["speed"] * dt / 3600.0

    # Python trigonometric functions use radians.
    heading_rad = math.radians(
        ac["heading"]
    )

    # Heading 0 is north; heading 90 is east.
    ac["x"] = (
        ac["x"]
        + distance_nm * math.sin(heading_rad)
    ) % SIZE

    ac["y"] = (
        ac["y"]
        + distance_nm * math.cos(heading_rad)
    ) % SIZE


# Shared client used to communicate with the Go gateway.
gateway = GatewayClient("simulator")


def send(fleet):
    """Send aircraft positions and receive committed clearances."""

    # Report claimed positions, not the hidden transmitter locations.
    claims = [
        {
            key: value
            for key, value in ac.items()
            if key != "emitter"
        }
        for ac in fleet
    ]

    return gateway.positions(claims)


# Thread pool lets receiver updates happen without blocking simulation.
_pool = ThreadPoolExecutor(
    max_workers=max(1, 2 * len(RECEIVERS))
)

_down = set()


def _broadcast_to(url, body):
    """Send one transmitter-position batch to a ground receiver."""
    request = urllib.request.Request(
        url + "/broadcast",
        data=body,
        headers={
            "Content-Type": "application/json"
        },
    )

    try:
        urllib.request.urlopen(
            request,
            timeout=0.5,
        ).read()

        # Report recovery when a previously unreachable receiver returns.
        if url in _down:
            _down.discard(url)
            print(
                f"receiver {url} reachable again",
                flush=True,
            )

    except (
        urllib.error.URLError,
        OSError,
    ) as error:

        # Avoid logging the same outage every tick.
        if url not in _down:
            _down.add(url)
            print(
                f"receiver {url} unreachable: {error}",
                flush=True,
            )


def broadcast(fleet):
    """Broadcast actual transmitter positions to ground receivers."""
    if not RECEIVERS:
        return

    rows = []

    for ac in fleet:
        if "seq" not in ac:
            continue

        # A spoofed aircraft transmits from its emitter coordinates.
        emitter_x, emitter_y = emitter_of(ac)

        rows.append({
            "callsign": ac["callsign"],
            "seq": ac["seq"],
            "emitter_x": emitter_x,
            "emitter_y": emitter_y,
        })

    body = json.dumps(rows).encode()

    # Each receiver receives its own asynchronous request.
    for url in RECEIVERS:
        _pool.submit(
            _broadcast_to,
            url,
            body,
        )


def send_safely(fleet, follow=False):
    """Send positions and report whether the gateway acknowledged them."""
    acknowledged = False

    try:
        # Send the aircraft batch to the gateway.
        instructions = send(fleet)

        # FIX #1: A successful gateway response counts as acknowledged.
        acknowledged = True

        # Follow controller clearances during live simulation.
        if follow:
            apply_instructions(
                fleet,
                instructions,
            )

    except (
        urllib.error.URLError,
        OSError,
    ) as error:

        # Communication failures are reported without stopping live mode.
        print(
            "gateway unreachable:",
            error,
            flush=True,
        )

    # Also send transmitter information to the receiver network.
    broadcast(fleet)

    # An acknowledgment means the gateway received the batch.
    # It does not confirm processing by every downstream Raft replica.
    return acknowledged


def run_live():
    """Run normal traffic generation or traffic recording."""
    rng = random.Random(SEED)

    # Use a scenario when one is configured.
    if SCENARIO:
        try:
            fleet = load_scenario(
                SCENARIO
            )

        except (
            OSError,
            ValueError,
        ) as error:
            raise SystemExit(
                f"Unable to load scenario: {error}"
            ) from error

    else:
        # Otherwise generate a random fleet.
        fleet = make_fleet(rng)

    ghost_count = sum(
        1
        for ac in fleet
        if "emitter" in ac
    )

    print(
        f"simulating {len(fleet)} aircraft "
        f"({ghost_count} spoofed), "
        f"tick {TICK}s -> {gateway.describe()}",
        flush=True,
    )

    if RECEIVERS:
        print(
            f"broadcasting to receivers: "
            f"{', '.join(RECEIVERS)}",
            flush=True,
        )

    recorder = None

    try:
        # In record mode, create the output JSONL file.
        if MODE == "record":
            path = resolve_path(
                RECORD_FILE
            )

            path.parent.mkdir(
                parents=True,
                exist_ok=True,
            )

            recorder = path.open(
                "w",
                encoding="utf-8",
            )

            print(
                f"recording aircraft frames to {path}",
                flush=True,
            )

        tick_number = 0

        while True:
            start = time.monotonic()

            # Move every aircraft once per simulation tick.
            for ac in fleet:
                step(ac, TICK)

            tick_number += 1

            # Recording occurs before network transmission.
            if recorder is not None:
                record_frame(
                    recorder,
                    tick_number,
                    fleet,
                )

            # Report positions and process new clearances.
            send_safely(
                fleet,
                follow=True,
            )

            # Sleep for the remainder of the configured tick interval.
            elapsed = time.monotonic() - start

            time.sleep(
                max(0.0, TICK - elapsed)
            )

    finally:
        # Close the recording file even when the simulator is stopped.
        if recorder is not None:
            recorder.close()


def run_replay():
    """Replay recorded frames and track successful gateway deliveries."""
    path = resolve_path(
        REPLAY_FILE
    )

    print(
        f"replaying aircraft frames from {path}",
        flush=True,
    )

    attempted = 0
    acknowledged = 0

    try:
        # Replay each frame exactly as recorded.
        for frame in replay_frames(path):
            start = time.monotonic()

            attempted += 1

            # FIX #1: Count an acknowledgment only after a successful send.
            if send_safely(frame["aircraft"]):
                acknowledged += 1

            # Replay uses original positions without calling step().
            elapsed = time.monotonic() - start

            time.sleep(
                max(0.0, TICK - elapsed)
            )

    except (
        OSError,
        ValueError,
    ) as error:

        raise SystemExit(
            f"Unable to replay recording: {error}"
        ) from error

    # FIX #1: Failed frames are counted separately.
    failed = attempted - acknowledged

    print(
        f"replay complete: {attempted} attempted, "
        f"{acknowledged} acknowledged, "
        f"{failed} failed",
        flush=True,
    )

    # Do not falsely report a successful replay after network failures.
    if failed:
        raise SystemExit(
            f"Replay failed to deliver {failed} frame(s)"
        )


def main():
    """Validate configuration and start the selected simulator mode."""

    # The simulation tick interval must be positive and finite.
    if TICK <= 0 or not math.isfinite(TICK):
        raise SystemExit(
            "TICK must be a positive, finite number"
        )

    # Only the three supported modes are permitted.
    if MODE not in (
        "live",
        "record",
        "replay",
    ):
        raise SystemExit(
            "MODE must be live, record, or replay"
        )

    if MODE == "replay":
        run_replay()
    else:
        run_live()


if __name__ == "__main__":
    main()
