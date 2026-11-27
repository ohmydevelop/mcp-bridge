"""QA bot replay contract and the original validation set."""
import json
import os
import sys
from pathlib import Path

FLAGS = ("auto_answer", "general_knowledge", "answer_ref")
SUITE = Path(__file__).with_name("qa_bot_validation_cases.json")


def original_cases(initial, name):
    return [(name,initial), ("missing",{}), ("mixed",dict(zip(FLAGS,[True,False,None]))),
            ("inverted",dict(zip(FLAGS,[False,True,True]))), ("unavailable",None)]


def parse_additions(doc):
    cases = doc["cases"]
    if not isinstance(cases,list) or not 1 <= len(cases) <= 12:
        raise ValueError("expected 1–12 additional bot snapshots")
    result = []
    for raw in cases:
        s = json.loads(raw)
        if not isinstance(s,dict) or any(k not in FLAGS or (v is not None and type(v) is not bool) for k,v in s.items()):
            raise ValueError("invalid QA bot snapshot")
        if s not in result:
            result.append(s)
    return result

def active_suite():
    seed=parse_additions(json.loads(SUITE.read_text()))
    path=os.getenv("QA_VALIDATION_DB")
    if not path:
        return None,0,seed
    sys.path.insert(0,str(Path(__file__).resolve().parents[1]/"examples"/"qa-maintenance"))
    from journal import Journal
    journal=Journal(path,seed)
    revision,cases=journal.suite()
    return journal,revision,cases
