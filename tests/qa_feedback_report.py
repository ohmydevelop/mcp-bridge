"""Export blinded QA answers for semantic review, then join those ratings to randomized arms."""
import argparse
from collections import Counter
import hashlib
import json
from math import comb
from pathlib import Path
import sqlite3
from contextlib import closing


def key(answer, flags):
    return hashlib.sha256(json.dumps([answer,flags],sort_keys=True).encode()).hexdigest()[:16]


def evidence(report):
    follow = report.get("followup",{})
    reads = follow.get("upstream",[])
    flags = reads[-1].get("flags") if reads else None
    answer = follow.get("answer")
    return key(answer,flags),answer,flags


def calls(row):
    return " → ".join(c["tool"]+"("+json.dumps(c["arguments"],ensure_ascii=False,separators=(",",":"))+")"
                      for c in row["calls"]) or "无"


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("directory",type=Path)
    parser.add_argument("--ratings",type=Path)
    args=parser.parse_args()
    reports={p.name:json.loads(p.read_text()) for p in sorted(args.directory.glob("screen-*.json"))}
    with closing(sqlite3.connect(args.directory/"trial.sqlite")) as db:
        slots=list(db.execute("SELECT id,arm,thread,report FROM allocation WHERE thread IS NOT NULL ORDER BY id"))
    blind={}
    for _,_,_,name in slots:
        k,answer,flags=evidence(reports[name])
        blind[k]={"answer":answer,"live_flags":flags}
    if not args.ratings:
        print(json.dumps(blind,sort_keys=True,indent=2,ensure_ascii=False))
        return
    ratings=json.loads(args.ratings.read_text())
    assert set(ratings)==set(blind) and all(type(v) is bool for v in ratings.values())
    counts=Counter()
    outcomes=[]
    for slot,arm,thread,name in slots:
        r=reports[name]
        s=r["followup"]
        assert r["arm"]==arm and r["goal"]==s["goal"] and s["thread"]==thread
        assert (s["injection"] is not None)==(arm=="advice")
        assert all(c["thread"]==thread and c["turn"]!=s["turn"] for c in r["calls"])
        k,answer,flags=evidence(r)
        passed=s["status"]=="completed" and len(s["upstream"])==1 and flags is not None and ratings[k]
        counts[arm]+=passed
        outcomes.append(dict(slot=slot,arm=arm,thread=thread,file=name,reads=len(s["upstream"]),passed=passed))
    by_arm=Counter(o["arm"] for o in outcomes)
    pairs=[outcomes[i:i+2] for i in range(0,len(outcomes)-1,2)]
    wins=Counter(next(x["arm"] for x in pair if x["passed"]) for pair in pairs if pair[0]["passed"]!=pair[1]["passed"])
    n=sum(wins.values())
    p=min(1,2*sum(comb(n,i) for i in range(min(wins["advice"],wins["control"])+1))/2**n)
    missing=[p.name for p in sorted(args.directory.glob("screen-*.log")) if p.with_suffix(".json").name not in reports]
    screened=len(reports)+len(missing)
    summary=dict(screened=screened,missing_traces=missing,allocated=dict(by_arm),successes=dict(counts),
                 discordant_pairs=dict(wins),paired_two_sided_p=p,outcomes=outcomes)
    (args.directory/"results.json").write_text(json.dumps(summary,ensure_ascii=False,indent=2))
    lines=["# QA 随机反馈实验逐轮记录","",f"共筛查 {screened} 个真实线程，随机入组 {len(outcomes)} 个。",
           "调用顺序取自 Bridge stdio 请求记录。读取次数取自 QA 请求转发端。","","| 样本 | 分组 | 轮次 | 真实 QA 读取 | MCP 工具及实参（按接收顺序） | 本地 shell 命令 | 最终回答 |",
           "|---|---|---:|---:|---|---|---|"]
    for name,r in reports.items():
        arm={"control":"不投递","advice":"投递"}.get(r.get("arm"),"未入组")
        for i,row in enumerate([r]+([r["followup"]] if "followup" in r else []),1):
            answer=row.get("answer") if i==2 else next((v.get("text") for v in reversed(r["codex_items"]) if v["type"]=="agent_message"),None)
            items=row["codex_items"] if i==1 else row["items"]
            commands=" → ".join(item.get("command","命令文本未留存").replace("|","&#124;") for item in items if item["type"] in ("command_execution","commandExecution")) or "无"
            lines.append(f'| [{name[7:9]}]({name}) | {arm} | {i} | {len(row["upstream"])} | `{calls(row).replace("|","&#124;")}` | `{commands}` | {(answer or "无").replace(chr(10)," ").replace("|","&#124;")} |')
    for name in missing:
        lines.append(f'| [{name[7:9]}]({name}) | 分组前运行失败 | 1 | 未留存 | 调用明细未留存，见错误日志 | 未留存 | 未留存 |')
    (args.directory/"calls.md").write_text("\n".join(lines)+"\n")
    print(json.dumps(summary,ensure_ascii=False))


if __name__=="__main__":
    main()
