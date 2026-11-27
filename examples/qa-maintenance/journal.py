"""Shared local QA evidence, event cursor and versioned validation suite."""
import json
import sqlite3
from contextlib import closing
from pathlib import Path


class Journal:
    def __init__(self,path,seed):
        self.path=Path(path)
        self.path.parent.mkdir(parents=True,exist_ok=True)
        with closing(self.connect()) as db, db:
            db.executescript("""
            CREATE TABLE IF NOT EXISTS suites(rev INTEGER PRIMARY KEY,cases TEXT NOT NULL);
            CREATE TABLE IF NOT EXISTS records(seq INTEGER PRIMARY KEY,run TEXT UNIQUE,rev INTEGER,body TEXT);
            CREATE TABLE IF NOT EXISTS jobs(run TEXT PRIMARY KEY,status TEXT,report TEXT);
            CREATE TABLE IF NOT EXISTS failures(run TEXT PRIMARY KEY,attempt INTEGER,reason TEXT);
            """)
            db.execute("INSERT OR IGNORE INTO suites VALUES(1,?)",(json.dumps(seed),))

    def connect(self):
        return sqlite3.connect(self.path,timeout=15)

    def suite(self):
        with closing(self.connect()) as db:
            rev,raw=db.execute("SELECT rev,cases FROM suites ORDER BY rev DESC LIMIT 1").fetchone()
            return rev,json.loads(raw)

    def record(self,run,rev,validation,later):
        body=json.dumps({"run":run,"validation":validation,"later_execution":later},sort_keys=True)
        with closing(self.connect()) as db, db:
            existing=db.execute("SELECT rev,body FROM records WHERE run=?",(run,)).fetchone()
            if existing and existing!=(rev,body):
                raise ValueError("conflicting evidence for run")
            db.execute("INSERT OR IGNORE INTO records(run,rev,body) VALUES(?,?,?)",(run,rev,body))

    def poll(self,cursor):
        with closing(self.connect()) as db:
            rows=db.execute("SELECT seq,run,rev FROM records WHERE seq>? ORDER BY seq LIMIT 50",(cursor,)).fetchall()
        return {"events":[{"eventId":run,"name":"qa.replay.completed","data":{"run":run,"revision":rev}}
                          for _,run,rev in rows],"cursor":str(rows[-1][0] if rows else cursor),
                "hasMore":len(rows)==50,"nextPollMs":1000}

    def failed_attempt(self,run,reason):
        with closing(self.connect()) as db,db:
            count=db.execute("""INSERT INTO failures VALUES (?,1,?) ON CONFLICT(run) DO UPDATE SET
                attempt=attempt+1,reason=excluded.reason RETURNING attempt""",(run,reason)).fetchone()[0]
            if count>=3:
                db.execute("UPDATE jobs SET status='failed',report=? WHERE run=? AND status='pending'",
                           (json.dumps({"reason":reason,"attempts":count}),run))

    def finish(self,run,rev,report,additions):
        with closing(self.connect()) as db, db:
            db.execute("BEGIN IMMEDIATE")
            if db.execute("SELECT status FROM jobs WHERE run=?",(run,)).fetchone()!=("pending",):
                return
            active,cases=db.execute("SELECT rev,cases FROM suites ORDER BY rev DESC LIMIT 1").fetchone()
            status=report["decision"]
            if active!=rev:
                status="stale"
            if status=="promoted":
                merged=json.loads(cases)
                merged.extend(v for v in additions if v not in merged)
                db.execute("INSERT INTO suites VALUES(?,?)",(rev+1,json.dumps(merged)))
            db.execute("UPDATE jobs SET status=?,report=? WHERE run=?",(status,json.dumps(report),run))
