---
id: p05-expected-before-anomaly
kind: convention
scope: [anomaly]
review_by: 2027-05-09
confidence: stated
source: "fixture: a recurring correction, \"that's actually expected\""
---
Agents don't know intent. Before calling something an anomaly, drift, bug or misconfiguration, look for a precedent or a documented reason. With no reason found, label it "unexplained, ask the owner" and don't assert it's wrong.

**how to check:** an anomaly claim whose evidence shows only *that* the thing differs, not that it was *unintended*, is `unverifiable`. If a matching precedent says it's intended, it is `contradicted`.
