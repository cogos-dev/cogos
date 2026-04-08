"""
Ablation Dashboard — live monitoring for the overnight Ralph experiment.

Single-file SPA: Python serves data + static HTML with embedded JS charts.
Auto-refreshes every 15 seconds.

Usage: python3 scripts/ablation-dashboard.py [--port 8421]
"""

import json
import os
import http.server
import socketserver
from pathlib import Path
from urllib.parse import urlparse

PORT = int(os.environ.get("DASHBOARD_PORT", "8421"))
CHAIN_FILE = Path(os.environ.get("CHAIN_FILE", "/tmp/ralph-overnight/chain.jsonl"))
LOG_FILE = Path(os.environ.get("LOG_FILE", "/tmp/ralph-overnight/ralph.log"))
SUPERVISOR_FILE = Path(os.environ.get("SUPERVISOR_FILE", "/tmp/ralph-overnight/supervisor-guidance.md"))

CONDITION_COLORS = {"A": "#e74c3c", "B": "#3498db", "C": "#2ecc71"}
CONDITION_NAMES = {"A": "Stock", "B": "RAG", "C": "Foveated"}


def read_chain():
    if not CHAIN_FILE.exists():
        return []
    entries = []
    for line in CHAIN_FILE.read_text().strip().split("\n"):
        if line.strip():
            try:
                entries.append(json.loads(line))
            except json.JSONDecodeError:
                pass
    return entries


def get_data():
    chain = read_chain()
    obs = [e for e in chain if e.get("type") == "observation"]
    supervisors = [e for e in chain if e.get("type") == "supervisor"]

    # Per-condition averages
    cond_stats = {}
    for cond in ["A", "B", "C"]:
        entries = [e for e in obs if e["condition"] == cond]
        if entries:
            scores = [e["score"] for e in entries]
            cond_stats[cond] = {
                "avg": round(sum(scores) / len(scores), 4),
                "n": len(entries),
                "min": round(min(scores), 3),
                "max": round(max(scores), 3),
            }

    # Per-question paired comparison
    questions = {}
    for e in obs:
        qid = e["question_id"]
        if qid not in questions:
            questions[qid] = {}
        cond = e["condition"]
        if cond not in questions[qid]:
            questions[qid][cond] = []
        questions[qid][cond].append(e["score"])

    paired = []
    for qid, conds in sorted(questions.items()):
        row = {"id": qid}
        for c in ["A", "B", "C"]:
            if c in conds:
                row[c] = round(sum(conds[c]) / len(conds[c]), 3)
                row[f"{c}_n"] = len(conds[c])
            else:
                row[c] = None
                row[f"{c}_n"] = 0
        # Differentials
        if row["A"] is not None and row["C"] is not None:
            row["C_vs_A"] = round(row["C"] - row["A"], 3)
        else:
            row["C_vs_A"] = None
        if row["A"] is not None and row["B"] is not None:
            row["B_vs_A"] = round(row["B"] - row["A"], 3)
        else:
            row["B_vs_A"] = None
        paired.append(row)

    # Timeline (score over observation index)
    timeline = []
    for i, e in enumerate(obs):
        timeline.append({
            "idx": i,
            "question_id": e["question_id"],
            "condition": e["condition"],
            "score": e["score"],
            "elapsed": e.get("elapsed_sec", 0),
            "cycle": e.get("cycle", 0),
        })

    # Supervisor notes
    sup_notes = []
    for s in supervisors:
        sup_notes.append({
            "cycle": s.get("cycle", 0),
            "stats": s.get("stats", {}),
            "analysis": s.get("analysis", "")[:300],
            "total_obs": s.get("total_observations", 0),
        })

    # Log tail
    log_tail = ""
    if LOG_FILE.exists():
        lines = LOG_FILE.read_text().strip().split("\n")
        log_tail = "\n".join(lines[-20:])

    # Supervisor guidance
    guidance = ""
    if SUPERVISOR_FILE.exists():
        guidance = SUPERVISOR_FILE.read_text()

    return {
        "total_observations": len(obs),
        "total_supervisors": len(supervisors),
        "conditions": cond_stats,
        "paired": paired,
        "timeline": timeline,
        "supervisors": sup_notes,
        "log_tail": log_tail,
        "guidance": guidance,
    }


HTML = """<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<title>CogOS Ablation Dashboard</title>
<meta http-equiv="refresh" content="15">
<script src="https://cdn.jsdelivr.net/npm/chart.js@4"></script>
<style>
  * { margin: 0; padding: 0; box-sizing: border-box; }
  body { font-family: 'SF Mono', 'Menlo', monospace; background: #0d1117; color: #c9d1d9; padding: 20px; }
  h1 { color: #58a6ff; margin-bottom: 8px; font-size: 1.4em; }
  h2 { color: #8b949e; margin: 16px 0 8px; font-size: 1.1em; border-bottom: 1px solid #21262d; padding-bottom: 4px; }
  .grid { display: grid; grid-template-columns: 1fr 1fr; gap: 16px; margin-bottom: 16px; }
  .card { background: #161b22; border: 1px solid #21262d; border-radius: 8px; padding: 16px; }
  .full { grid-column: 1 / -1; }
  .stat-row { display: flex; gap: 16px; margin-bottom: 12px; }
  .stat { flex: 1; text-align: center; padding: 12px; border-radius: 6px; }
  .stat .value { font-size: 2em; font-weight: bold; }
  .stat .label { font-size: 0.8em; color: #8b949e; }
  .stat-a { background: rgba(231,76,60,0.15); border: 1px solid rgba(231,76,60,0.3); }
  .stat-b { background: rgba(52,152,219,0.15); border: 1px solid rgba(52,152,219,0.3); }
  .stat-c { background: rgba(46,204,113,0.15); border: 1px solid rgba(46,204,113,0.3); }
  .stat-a .value { color: #e74c3c; }
  .stat-b .value { color: #3498db; }
  .stat-c .value { color: #2ecc71; }
  table { width: 100%; border-collapse: collapse; font-size: 0.85em; }
  th { color: #8b949e; text-align: left; padding: 6px 8px; border-bottom: 1px solid #21262d; }
  td { padding: 6px 8px; border-bottom: 1px solid #21262d; }
  .pos { color: #2ecc71; }
  .neg { color: #e74c3c; }
  .neutral { color: #8b949e; }
  pre { background: #0d1117; padding: 12px; border-radius: 6px; overflow-x: auto; font-size: 0.8em; line-height: 1.5; max-height: 300px; overflow-y: auto; white-space: pre-wrap; word-wrap: break-word; }
  canvas { max-height: 250px; }
  .meta { color: #8b949e; font-size: 0.85em; margin-bottom: 8px; }
  .diff-bar { display: inline-block; height: 14px; border-radius: 2px; vertical-align: middle; }
</style>
</head>
<body>
<h1>CogOS Ablation Dashboard</h1>
<div class="meta" id="meta"></div>

<div class="stat-row">
  <div class="stat stat-a" id="stat-a"><div class="value">—</div><div class="label">A: Stock (no context)</div></div>
  <div class="stat stat-b" id="stat-b"><div class="value">—</div><div class="label">B: RAG (keyword search)</div></div>
  <div class="stat stat-c" id="stat-c"><div class="value">—</div><div class="label">C: Foveated (CogOS)</div></div>
</div>

<div class="grid">
  <div class="card">
    <h2>Score Timeline</h2>
    <canvas id="timeline"></canvas>
  </div>
  <div class="card">
    <h2>Condition Averages</h2>
    <canvas id="barChart"></canvas>
  </div>
  <div class="card full">
    <h2>Paired Comparison (per question)</h2>
    <table id="paired-table">
      <thead><tr><th>Question</th><th>A (Stock)</th><th>B (RAG)</th><th>C (Foveated)</th><th>C vs A</th><th>B vs A</th></tr></thead>
      <tbody></tbody>
    </table>
  </div>
  <div class="card">
    <h2>Supervisor Guidance</h2>
    <pre id="guidance">waiting for first barge-in...</pre>
  </div>
  <div class="card">
    <h2>Live Log</h2>
    <pre id="log">loading...</pre>
  </div>
</div>

<script>
let timelineChart = null;
let barChartObj = null;

async function refresh() {
  const resp = await fetch('/api/data');
  const d = await resp.json();

  // Meta
  document.getElementById('meta').textContent =
    `${d.total_observations} observations | ${d.total_supervisors} supervisor barge-ins | auto-refresh 15s`;

  // Stat cards
  for (const [c, cls] of [['A','stat-a'],['B','stat-b'],['C','stat-c']]) {
    const el = document.getElementById(cls);
    const s = d.conditions[c];
    if (s) {
      el.querySelector('.value').textContent = s.avg.toFixed(3);
      el.querySelector('.label').textContent =
        `${c}: ${{'A':'Stock','B':'RAG','C':'Foveated'}[c]} (n=${s.n})`;
    }
  }

  // Timeline
  const ctx1 = document.getElementById('timeline').getContext('2d');
  const datasets = {};
  for (const pt of d.timeline) {
    if (!datasets[pt.condition]) datasets[pt.condition] = { label: pt.condition, data: [], borderColor: {'A':'#e74c3c','B':'#3498db','C':'#2ecc71'}[pt.condition], backgroundColor: 'transparent', pointRadius: 3, tension: 0.1 };
    datasets[pt.condition].data.push({ x: pt.idx, y: pt.score });
  }
  if (timelineChart) timelineChart.destroy();
  timelineChart = new Chart(ctx1, {
    type: 'scatter',
    data: { datasets: Object.values(datasets) },
    options: { scales: { x: { title: { display: true, text: 'Observation #', color: '#8b949e' }, ticks: { color: '#8b949e' }, grid: { color: '#21262d' } }, y: { min: 0, max: 1, title: { display: true, text: 'Score', color: '#8b949e' }, ticks: { color: '#8b949e' }, grid: { color: '#21262d' } } }, plugins: { legend: { labels: { color: '#c9d1d9' } } } }
  });

  // Bar chart
  const ctx2 = document.getElementById('barChart').getContext('2d');
  const conds = ['A','B','C'];
  const avgs = conds.map(c => d.conditions[c] ? d.conditions[c].avg : 0);
  if (barChartObj) barChartObj.destroy();
  barChartObj = new Chart(ctx2, {
    type: 'bar',
    data: { labels: ['Stock (A)', 'RAG (B)', 'Foveated (C)'], datasets: [{ data: avgs, backgroundColor: ['#e74c3c', '#3498db', '#2ecc71'] }] },
    options: { scales: { y: { min: 0, max: 1, ticks: { color: '#8b949e' }, grid: { color: '#21262d' } }, x: { ticks: { color: '#8b949e' }, grid: { color: '#21262d' } } }, plugins: { legend: { display: false } } }
  });

  // Paired table
  const tbody = document.querySelector('#paired-table tbody');
  tbody.innerHTML = '';
  for (const row of d.paired) {
    const tr = document.createElement('tr');
    const diffCell = (val) => {
      if (val === null) return '<td class="neutral">—</td>';
      const cls = val > 0.05 ? 'pos' : val < -0.05 ? 'neg' : 'neutral';
      const width = Math.min(Math.abs(val) * 100, 50);
      const color = val > 0 ? '#2ecc71' : '#e74c3c';
      return `<td class="${cls}">${val > 0 ? '+' : ''}${val.toFixed(3)} <span class="diff-bar" style="width:${width}px;background:${color}"></span></td>`;
    };
    tr.innerHTML = `
      <td>${row.id}</td>
      <td>${row.A !== null ? row.A.toFixed(3) : '—'}</td>
      <td>${row.B !== null ? row.B.toFixed(3) : '—'}</td>
      <td>${row.C !== null ? row.C.toFixed(3) : '—'}</td>
      ${diffCell(row.C_vs_A)}
      ${diffCell(row.B_vs_A)}
    `;
    tbody.appendChild(tr);
  }

  // Supervisor
  document.getElementById('guidance').textContent = d.guidance || 'waiting for first barge-in...';

  // Log
  document.getElementById('log').textContent = d.log_tail || 'no log yet';
}

refresh();
</script>
</body>
</html>"""


class Handler(http.server.SimpleHTTPRequestHandler):
    def do_GET(self):
        parsed = urlparse(self.path)
        if parsed.path == "/api/data":
            data = get_data()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Access-Control-Allow-Origin", "*")
            self.end_headers()
            self.wfile.write(json.dumps(data).encode())
        elif parsed.path == "/" or parsed.path == "/index.html":
            self.send_response(200)
            self.send_header("Content-Type", "text/html")
            self.end_headers()
            self.wfile.write(HTML.encode())
        else:
            self.send_error(404)

    def log_message(self, format, *args):
        pass  # Suppress access logs


if __name__ == "__main__":
    import argparse
    parser = argparse.ArgumentParser()
    parser.add_argument("--port", type=int, default=PORT)
    args = parser.parse_args()

    with socketserver.TCPServer(("", args.port), Handler) as httpd:
        print(f"Ablation Dashboard: http://localhost:{args.port}")
        print(f"Chain: {CHAIN_FILE}")
        print(f"Press Ctrl+C to stop")
        try:
            httpd.serve_forever()
        except KeyboardInterrupt:
            print("\nStopped.")
