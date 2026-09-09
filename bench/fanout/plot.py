#!/usr/bin/env python3
# plot.py — render docs/assets/fanout.svg from bench/results/fanout-v0.2.csv.
# Two panels (cost vs N, wall-clock vs N), lith and copy on each, log-x.
import csv, os, sys
import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt

HERE = os.path.dirname(os.path.abspath(__file__))
CSV = os.path.join(HERE, "..", "results", "fanout-v0.2.csv")
OUT = os.path.join(HERE, "..", "..", "docs", "assets", "fanout.svg")

def load():
    rows = []
    with open(CSV) as f:
        for line in f:
            if line.startswith("#") or not line.strip():
                continue
            rows.append(line)
    r = csv.DictReader(rows)
    data = {"lith": {}, "copy": {}, "copy-best": {}}
    for row in r:
        data[row["path"]][int(row["n"])] = {
            "cost": float(row["cost_usd"]),
            "wall": float(row["wall_s"]),
            "complete": row.get("complete", "1") == "1",
        }
    return data

def series(d, key):
    ns = sorted(d)
    return ns, [d[n][key] for n in ns]

def main():
    d = load()
    fig, (ax1, ax2) = plt.subplots(1, 2, figsize=(10, 4.2))
    C = {"lith": "#2a7ae2", "copy": "#d1495b", "copy-best": "#e08e0b"}
    for path in ("lith", "copy", "copy-best"):
        if not d[path]:
            continue
        comp = sorted(n for n in d[path] if d[path][n]["complete"])
        inc = sorted(n for n in d[path] if not d[path][n]["complete"])
        ax1.plot(comp, [d[path][n]["cost"] for n in comp], "o-", color=C[path], label=path, linewidth=2, markersize=6)
        ax2.plot(comp, [d[path][n]["wall"]/60 for n in comp], "o-", color=C[path], label=path, linewidth=2, markersize=6)
        for n in inc:
            ax1.plot([n], [d[path][n]["cost"]], "x", color=C[path], markersize=9, markeredgewidth=2)
            ax2.plot([n], [d[path][n]["wall"]/60], "x", color=C[path], markersize=9, markeredgewidth=2)
            ax2.annotate("N=1 did not\nfinish (>2.5 h)", (n, d[path][n]["wall"]/60), textcoords="offset points",
                         xytext=(12, -6), fontsize=8, color=C[path])
    for ax in (ax1, ax2):
        ax.set_xscale("log", base=2)
        ax.set_xlabel("nodes (N)")
        ax.set_xticks([1, 8, 64])
        ax.set_xticklabels(["1", "8", "64"])
        ax.grid(True, which="both", alpha=0.25)
        ax.legend()
    ax1.set_ylabel("cost to result (USD)")
    ax1.set_title("Cost vs width")
    ax2.set_ylabel("wall-clock to result (min)")
    ax2.set_title("Wall-clock vs width")
    fig.suptitle("Fan-out: 64 CRAMs (flagstat, 534 GB), c8g.2xlarge, us-east-1", fontsize=11)
    fig.tight_layout()
    os.makedirs(os.path.dirname(OUT), exist_ok=True)
    fig.savefig(OUT, format="svg", bbox_inches="tight")
    print("wrote", OUT)

if __name__ == "__main__":
    main()
