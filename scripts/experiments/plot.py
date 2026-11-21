#!/usr/bin/env python3
"""Make the report's graphs from the newest results/<experiment>-*.json files.

    pip install matplotlib
    python3 scripts/experiments/plot.py            # writes results/figures/*.png
    python3 scripts/experiments/plot.py --summary  # also prints the key numbers

Each graph is one PNG (200 dpi, sized for a report column). The numbers behind
every graph are in the JSON file it was made from.
"""
import argparse
import glob
import json
import os
import statistics as st

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt  # noqa: E402

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
RESULTS = os.path.join(ROOT, "results")
FIG = os.path.join(RESULTS, "figures")

# Fixed categorical order (colour follows the entity, never its rank).
SERIES = ["#2a78d6", "#eb6834", "#1baf7a", "#eda100", "#e87ba4", "#008300", "#6250d6", "#e34948"]
SECTOR_COLOR = {"sector-1": SERIES[0], "sector-2": SERIES[1], "sector-3": SERIES[2],
                "sector-4": SERIES[3], "standby": SERIES[4], "tower": SERIES[5], "registry": SERIES[6]}
STATUS = {"good": "#0ca30c", "warning": "#fab219", "critical": "#d03b3b"}
INK, INK2, GRID, SURFACE = "#0b0b0b", "#52514e", "#e4e3df", "#fcfcfb"

plt.rcParams.update({
    "figure.facecolor": SURFACE, "axes.facecolor": SURFACE, "savefig.facecolor": SURFACE,
    "axes.edgecolor": GRID, "axes.labelcolor": INK2, "xtick.color": INK2, "ytick.color": INK2,
    "text.color": INK, "axes.grid": True, "grid.color": GRID, "grid.linewidth": 0.8,
    "axes.spines.top": False, "axes.spines.right": False, "font.size": 9, "axes.titlesize": 10,
    "axes.titleweight": "bold", "axes.titlelocation": "left", "legend.frameon": False, "lines.linewidth": 2,
})


def newest(name):
    files = sorted(glob.glob(os.path.join(RESULTS, f"{name}-*.json")))
    if not files:
        return None, None
    with open(files[-1]) as f:
        return json.load(f), os.path.basename(files[-1])


def save(fig, name):
    os.makedirs(FIG, exist_ok=True)
    path = os.path.join(FIG, name)
    fig.savefig(path, dpi=200, bbox_inches="tight")
    plt.close(fig)
    print("wrote", os.path.relpath(path, ROOT))


# ---------- failover ----------

def plot_failover(d, summary):
    rows = [r for r in d["rows"] if r["election_s"] is not None]
    clusters = [c for c in ["sector-1", "sector-2", "sector-3", "sector-4", "tower", "registry"]
                if any(r["cluster"] == c for r in rows)]
    fig, axes = plt.subplots(1, 2, figsize=(7.5, 3), sharey=False)
    for ax, key, title in [(axes[0], "election_s", "New leader elected after a leader crash"),
                           (axes[1], "catchup_s", "Restarted node catches up from its log")]:
        for i, c in enumerate(clusters):
            vals = [r[key] for r in rows if r["cluster"] == c and r[key] is not None]
            if not vals:
                continue
            ax.bar(i, st.mean(vals), width=0.6, color=SECTOR_COLOR[c], zorder=2)
            ax.scatter([i] * len(vals), vals, s=14, color=INK, zorder=3, linewidths=0)
        ax.set_xticks(range(len(clusters)), clusters, rotation=30, ha="right")
        ax.set_ylabel("seconds")
        ax.set_title(title)
    fig.text(0, -0.08, "Bars: mean. Dots: each crash. Gateway polls every 500 ms, so times are accurate to about ±0.5 s.",
             color=INK2, fontsize=8)
    save(fig, "failover.png")
    el = [r["election_s"] for r in rows]
    cu = [r["catchup_s"] for r in rows if r["catchup_s"] is not None]
    clr = [r["clearance"] for r in d["rows"] if r.get("clearance")]
    summary["failover"] = {
        "crashes": len(d["rows"]), "elected": len(rows),
        "election_mean_s": round(st.mean(el), 2), "election_max_s": round(max(el), 2),
        "catchup_mean_s": round(st.mean(cu), 2) if cu else None,
        "clearances_sent_during_election": len(clr),
        "clearances_committed": sum(1 for c in clr if c.get("final") == "committed"),
        "clearance_wait_mean_s": round(st.mean([c["wait_s"] for c in clr]), 2) if clr else None,
        "safety_ok": (d.get("safety") or {}).get("ok"),
    }


# ---------- scaling ----------

def plot_scaling(d, summary):
    runs = sorted(d["runs"], key=lambda r: r["aircraft"])
    n = [r["aircraft"] for r in runs]
    fig, axes = plt.subplots(1, 3, figsize=(9, 2.8))
    panels = [("mean_handoff_ms", "Handoff latency (mean, ms)"),
              ("handoffs_per_min", "Handoffs per minute"),
              ("gateway_cpu_avg", "Gateway CPU (%)")]
    for ax, (key, title) in zip(axes, panels):
        ys = [r[key] for r in runs]
        ax.plot(n, ys, color=SERIES[0], marker="o", markersize=5, zorder=3)
        for x, y in zip(n, ys):
            if y is not None:
                ax.annotate(f"{y:.0f}" if y >= 10 else f"{y:.1f}", (x, y), textcoords="offset points",
                            xytext=(0, 6), ha="center", fontsize=8, color=INK2)
        ax.set_xticks(n)
        ax.set_xlabel("aircraft")
        ax.set_title(title)
        ax.set_ylim(0, max(y for y in ys if y is not None) * 1.2)
    save(fig, "scaling.png")
    if all(r.get("conflict_pairs_checked_avg") is not None for r in runs):
        fig, ax = plt.subplots(figsize=(4.5, 2.8))
        grid = [r["conflict_pairs_checked_avg"] for r in runs]
        allp = [r["conflict_all_pairs_avg"] for r in runs]
        ax.plot(n, allp, color=SERIES[1], marker="o", markersize=5, label="all pairs, n(n-1)/2")
        ax.plot(n, grid, color=SERIES[0], marker="o", markersize=5, label="pairs the grid checks")
        for x, y in zip(n, grid):
            ax.annotate(f"{y:.0f}", (x, y), textcoords="offset points", xytext=(0, 6), ha="center", fontsize=8, color=INK2)
        ax.set_xticks(n)
        ax.set_xlabel("aircraft")
        ax.set_title("Conflict detection: pairs checked per second")
        ax.legend(fontsize=8)
        save(fig, "conflict_grid.png")
    summary["scaling"] = [{k: (round(r[k], 1) if isinstance(r[k], float) else r[k])
                           for k in ("aircraft", "handoffs_per_min", "mean_handoff_ms", "multicast_per_s",
                                     "aborted", "gateway_cpu_avg", "leader_cpu_avg")
                           if k in r} |
                          {k: round(r[k], 3) for k in ("conflict_pairs_checked_avg", "conflict_all_pairs_avg",
                                                       "conflict_ms_avg") if r.get(k) is not None} |
                          {"safety_ok": (r.get("safety") or {}).get("ok")} for r in runs]


# ---------- conflict ----------

def plot_conflict(d, summary):
    """One row per watched pair: horizontal and vertical distance over time,
    with the conflict left alone vs resolved with the suggested clearance."""
    runs = {("resolved" if r["resolve"] else "ignored"): r for r in sorted(d["runs"], key=lambda r: r["resolve"])}
    keys = ["CONV001|CONV002", "CROSS01|CROSS02"]
    names = {"CONV001|CONV002": "Head-on pair", "CROSS01|CROSS02": "Crossing pair"}
    style = {"ignored": (SERIES[1], "left alone"), "resolved": (SERIES[0], "suggested clearance issued")}
    fig, axes = plt.subplots(2, 2, figsize=(9, 5.2), sharex=True)
    for row, key in enumerate(keys):
        for label, r in runs.items():
            color, text = style[label]
            pts = [(t["t"], t[key]["h_nm"], t[key]["v_ft"]) for t in r["track"] if t.get(key, {}).get("h_nm") is not None]
            if not pts:
                continue
            ts, hs, vs = zip(*pts)
            width = 3.5 if label == "ignored" else 1.8  # the wider line stays visible under the other
            axes[row][0].plot(ts, hs, color=color, label=text, linewidth=width)
            axes[row][1].plot(ts, vs, color=color, label=text, linewidth=width)
            p = r["pairs"].get(key)
            if p:
                for ax in axes[row]:
                    ax.axvline(p["first_t"], color=INK2, linewidth=1, linestyle=":")
                if p.get("issued"):
                    for ax in axes[row]:
                        ax.axvline(p["issued"]["t"], color=color, linewidth=1, linestyle="--")
        axes[row][0].axhline(5, color=STATUS["critical"], linewidth=1, linestyle="--")
        axes[row][1].axhline(1000, color=STATUS["critical"], linewidth=1, linestyle="--")
        axes[row][0].set_title(f"{names[key]}: horizontal distance (NM)")
        axes[row][1].set_title(f"{names[key]}: vertical separation (ft)")
        axes[row][0].set_ylim(bottom=0)
        axes[row][1].set_ylim(bottom=0)
    for ax in axes[1]:
        ax.set_xlabel("seconds since the simulator started")
    axes[0][1].legend(loc="upper left", fontsize=8)
    fig.text(0.01, -0.02, "Red dashed: separation minimum (5 NM, 1,000 ft). Dotted: conflict alerted. "
             "Dashed: suggested clearance issued.\nThe resolutions are climbs, so the horizontal tracks "
             "are the same in both runs. A loss needs both distances under the minimum at once.",
             fontsize=8, color=INK2)
    fig.tight_layout()
    save(fig, "conflict.png")
    out = {"react_s": d.get("react_s")}
    for label, r in runs.items():
        out[label] = {k: {"alerted_t": round(p["first_t"], 1), "warning_s": round(p["first_in_s"], 1),
                          "loss": p["loss"], "issued": (p.get("issued") or {}).get("instruction")}
                      for k, p in r["pairs"].items()}
        out[label]["stats"] = r.get("stats")
        out[label]["safety_ok"] = (r.get("safety") or {}).get("ok")
    summary["conflict"] = out


# ---------- transport ----------

def plot_transport(d, summary):
    runs = d["runs"]
    labels = [r["label"] for r in runs]
    colors = [SERIES[i] for i in range(len(runs))]  # one colour per transport, fixed order
    fig, axes = plt.subplots(1, 3, figsize=(9, 2.8))

    ax = axes[0]  # clearance latency: box per transport
    data = [r["clearance_ms"] or [0] for r in runs]
    bp = ax.boxplot(data, widths=0.5, showfliers=False, patch_artist=True, medianprops={"color": INK})
    for box, c in zip(bp["boxes"], colors):
        box.set_facecolor(c)
        box.set_alpha(0.75)
        box.set_edgecolor(c)
    for i, r in enumerate(runs, 1):
        if r["clearance_p50_ms"] is not None:
            ax.annotate(f"{r['clearance_p50_ms']:.1f}", (i, r["clearance_p50_ms"]), textcoords="offset points",
                        xytext=(16, -3), fontsize=8, color=INK2)
    ax.set_xticks(range(1, len(runs) + 1), labels)
    ax.set_title("Clearance round trip (ms)")

    for ax, (key, title) in zip(axes[1:], [("mean_handoff_ms", "Handoff latency (mean, ms)"),
                                            ("gateway_cpu_avg", "Gateway CPU (%)")]):
        ys = [r[key] for r in runs]
        ax.bar(labels, [y or 0 for y in ys], color=colors, width=0.55, zorder=3)
        for x, (y, r) in enumerate(zip(ys, runs)):
            # Missing is not zero: say so instead of drawing a 0 bar.
            text = "no data" if y is None else f"{y:.1f}"
            if key == "mean_handoff_ms":
                text += f"\nn={r['handoffs']}"
            ax.annotate(text, (x, y or 0), textcoords="offset points", xytext=(0, 3), ha="center", fontsize=8, color=INK2)
        ax.set_title(title)
        ax.set_ylim(0, max([y for y in ys if y is not None] + [1]) * 1.35)
    for ax in axes:
        ax.tick_params(axis="x", labelsize=8)
        ax.grid(axis="x", visible=False)
    save(fig, "transport.png")
    summary["transport"] = [{k: (round(r[k], 2) if isinstance(r[k], float) else r[k])
                             for k in ("label", "aircraft", "clearances", "clearance_p50_ms", "clearance_p95_ms",
                                       "handoffs", "mean_handoff_ms", "multicast_per_s", "multicast_dropped",
                                       "gateway_cpu_avg")} |
                            {"safety_ok": (r.get("safety") or {}).get("ok")} for r in runs]


# ---------- surge ----------

def plot_surge(d, summary):
    s = d["samples"]
    names = [x for x in ["sector-1", "sector-2", "sector-3", "sector-4", "standby"]
             if any(x in p["sectors"] for p in s)]
    fig, ax = plt.subplots(figsize=(7.5, 3.2))
    t = [p["t"] for p in s]
    for name in names:
        ax.plot(t, [p["sectors"].get(name, {}).get("load", 0) for p in s], color=SECTOR_COLOR[name], label=name)
    cfg = d.get("config") or {}
    for key, label in (("split_at", "split threshold"), ("merge_at", "merge threshold")):
        if cfg.get(key):
            ax.axhline(cfg[key], color=INK2, linewidth=1, linestyle=(0, (4, 3)))
            ax.text(t[-1], cfg[key], f" {label} ({cfg[key]})", va="bottom", ha="right", fontsize=8, color=INK2)
    # One label per burst of events (a drain and its merge are about 1 s apart).
    marks = []
    for e in d["events"]:
        if e["kind"] not in ("split", "merge", "drain"):
            continue
        ax.axvline(e["t"], color=INK, linewidth=1)
        if marks and e["t"] - marks[-1][0] < 5:
            marks[-1][1].append(e["kind"])
        else:
            marks.append([e["t"], [e["kind"]], e["sector"]])
    top = ax.get_ylim()[1]
    for t0, kinds, sector in marks:
        ax.text(t0 + 1, top, f"{' + '.join(kinds)} {sector}", va="top", ha="left", fontsize=8, color=INK)
    ax.set_xlabel("seconds since start")
    ax.set_ylabel("aircraft controlled")
    ax.set_title("Elastic sectors: 60 aircraft surge into sector-1, then spread out")
    ax.legend(ncol=len(names), loc="upper center", bbox_to_anchor=(0.5, -0.2))
    save(fig, "surge.png")
    ev = [{"t": round(e["t"], 1), "kind": e["kind"], "sector": e["sector"], "detail": e.get("detail")}
          for e in d["events"]]
    summary["surge"] = {"events": ev, "aborted_handoffs": s[-1]["aborted"], "handoffs": s[-1]["handoffs"],
                        "peak_load": {n: max(p["sectors"].get(n, {}).get("load", 0) for p in s) for n in names},
                        "safety_ok": (d.get("safety") or {}).get("ok")}


# ---------- ghost ----------

def plot_ghost(d, summary):
    s = d["samples"]
    t = [p["t"] for p in s]
    fig, ax = plt.subplots(figsize=(7.5, 3))
    for key, color, label in (("VERIFIED", STATUS["good"], "verified"),
                              ("UNVERIFIED", STATUS["warning"], "unverified"),
                              ("SUSPECT", STATUS["critical"], "suspect (ghost)")):
        ax.plot(t, [p.get(key, 0) for p in s], color=color, label=label)
    # shade the phase with a receiver down
    down = [p["t"] for p in s if p["phase"] == "receiver-b stopped"]
    if down:
        ax.axvspan(min(down), max(down), color=GRID, alpha=0.6, zorder=0, linewidth=0)
        ax.text((min(down) + max(down)) / 2, ax.get_ylim()[1] * 0.97, "receiver-b stopped", ha="center", va="top",
                fontsize=8, color=INK2)
    ax.set_xlabel("seconds since start")
    ax.set_ylabel("aircraft")
    ax.set_title(f"Position verification: {d['real_aircraft']} real aircraft and {d['ghosts']} spoofed")
    ax.legend(ncol=3, loc="upper center", bbox_to_anchor=(0.5, -0.2))
    save(fig, "ghost.png")
    det = [v for v in d["detection_s"].values() if v is not None]
    summary["ghost"] = {"ghosts": d["ghosts"], "detected": d["detected"],
                        "detection_mean_s": round(st.mean(det), 1) if det else None,
                        "detection_max_s": round(max(det), 1) if det else None,
                        "false_positives": len(d["false_positives"]), "real_aircraft": d["real_aircraft"],
                        "max_unverified_with_receiver_down": max((p["UNVERIFIED"] for p in s
                                                                  if p["phase"] == "receiver-b stopped"), default=None),
                        "safety_ok": (d.get("safety") or {}).get("ok")}


# ---------- chaos ----------

def plot_chaos(d, summary):
    fig, ax = plt.subplots(figsize=(7.5, 3))
    # Status in the gateway's first answer. "pending" ones were buffered while
    # a cluster had no leader; the final counts are in clearance_stats.
    groups = (("committed", STATUS["good"]), ("pending", STATUS["warning"]), ("rejected", STATUS["critical"]))
    lanes = {g: i for i, (g, _) in enumerate(groups)}
    for g, color in groups:
        pts = [o["t"] for o in d["outcomes"] if o["status"] == g]
        ax.scatter(pts, [lanes[g]] * len(pts), s=10, color=color, linewidths=0, zorder=3)
    for a in d["actions"]:
        ax.axvline(a["t"], color=INK2, linewidth=1, zorder=1)
        ax.text(a["t"], len(groups) - 0.4, " " + a["action"].split(" leader")[0].replace("sector-", "s"),
                rotation=90, va="bottom", ha="right", fontsize=7, color=INK2)
    names = {"committed": "committed at once", "pending": "buffered, then committed", "rejected": "rejected"}
    fin = d.get("clearance_stats") or {}
    ax.set_yticks(range(len(groups)), [f"{names[g]} ({d['clearances'].get(g, 0)})" for g, _ in groups])
    fig.text(0, -0.1, f"Final: {fin.get('committed', '?')} committed, {fin.get('rejected', '?')} rejected, "
             f"{fin.get('expired', '?')} expired. Vertical lines: injected faults.", color=INK2, fontsize=8)
    ax.set_ylim(-0.6, len(groups) + 3.2)
    ax.set_xlabel("seconds since start")
    ok = (d.get("safety") or {}).get("ok")
    ax.set_title("Clearances every 0.5 s during faults · safety invariants: " + ("all held" if ok else "VIOLATED"))
    ax.grid(axis="y", visible=False)
    save(fig, "chaos.png")
    inv = (d.get("safety") or {}).get("invariants", {})
    summary["chaos"] = {"clearances_first_answer": d["clearances"], "clearances_final": d.get("clearance_stats"), "faults": [a["action"] for a in d["actions"]],
                        "safety_ok": ok,
                        "invariants": {k: f"{v['checks']} checks, {v['violations']} violations" for k, v in inv.items()}}


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--summary", action="store_true", help="print the key numbers as JSON")
    args = p.parse_args()
    summary = {}
    for name, fn in (("failover", plot_failover), ("scaling", plot_scaling), ("surge", plot_surge),
                     ("ghost", plot_ghost), ("chaos", plot_chaos), ("transport", plot_transport),
                     ("conflict", plot_conflict)):
        d, src = newest(name)
        if d is None:
            print(f"no results/{name}-*.json yet, skipping")
            continue
        fn(d, summary)
        summary.setdefault("sources", {})[name] = src
    os.makedirs(FIG, exist_ok=True)
    with open(os.path.join(FIG, "summary.json"), "w") as f:
        json.dump(summary, f, indent=1)
    if args.summary:
        print(json.dumps(summary, indent=1))


if __name__ == "__main__":
    main()
