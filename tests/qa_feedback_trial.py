"""Randomize only verified natural repeats. Each subprocess owns its Bridge and Codex state."""
import argparse
from concurrent.futures import ThreadPoolExecutor
from contextlib import closing
import json
from pathlib import Path
import random
import secrets
import sqlite3
import subprocess
import sys


def enroll(database, thread, report):
    with closing(sqlite3.connect(database)) as db, db:
        db.execute("BEGIN IMMEDIATE")
        slot = db.execute("SELECT id,arm FROM allocation WHERE thread IS NULL ORDER BY id LIMIT 1").fetchone()
        if not slot:
            raise RuntimeError("no remaining trial slots")
        db.execute("UPDATE allocation SET thread=?,report=? WHERE id=?", (thread, report, slot[0]))
        return slot[1], db.execute("SELECT advice FROM protocol").fetchone()[0]


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--codex", required=True)
    parser.add_argument("--resume", action="store_true")
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=args.resume)
    database = args.output/"trial.sqlite"
    advice = json.loads(Path("bin/trace/qa-natural-thread-feedback-2.json").read_text())["followup"]["injection"]["advice"]
    seed = secrets.randbits(64)
    rng = random.Random(seed)
    target, limit = 12, 36
    arms = []
    for _ in range(target//2):
        pair = ["advice","control"]
        rng.shuffle(pair)
        arms.extend(pair)
    if not args.resume:
        (args.output/"protocol.json").write_text(json.dumps(dict(seed=seed,allocation=arms,advice=advice,
            target=target,screen_limit=limit,outcome="Completed second turn, exactly one live request, successful response, and three correct flags in final answer. All allocated threads in denominator."),ensure_ascii=False,indent=2))
        with closing(sqlite3.connect(database)) as db, db:
            db.execute("CREATE TABLE allocation(id INTEGER PRIMARY KEY, arm TEXT,thread TEXT UNIQUE,report TEXT)")
            db.execute("CREATE TABLE protocol(advice TEXT)")
            db.execute("INSERT INTO protocol VALUES (?)",(advice,))
            db.executemany("INSERT INTO allocation(arm) VALUES (?)",[(arm,) for arm in arms])
    else:
        saved = json.loads((args.output/"protocol.json").read_text())
        target, limit = saved["target"], saved["screen_limit"]
    def run(n):
        output = args.output/f"screen-{n:02}.json"
        with output.with_suffix(".log").open("w") as log:
            return subprocess.run([sys.executable,"tests/qa_natural.py","--trial",str(database),
                                   "--codex",args.codex,"--output",str(output)],stdout=log,stderr=log).returncode
    screened = len(list(args.output.glob("screen-*.log")))
    with ThreadPoolExecutor(max_workers=4) as pool, closing(sqlite3.connect(database)) as db:
        while screened < limit:
            remaining = db.execute("SELECT count(*) FROM allocation WHERE thread IS NULL").fetchone()[0]
            count = min(4, remaining, limit-screened)
            if not count:
                break
            codes = list(pool.map(run,range(screened+1, screened+count+1)))
            screened += count
            print("screened",screened,"allocated",db.execute("SELECT count(thread) FROM allocation").fetchone()[0],"exit",codes,flush=True)
            if any(codes):
                raise RuntimeError("trial execution failed; retain allocations and inspect logs")


if __name__=="__main__":
    main()
