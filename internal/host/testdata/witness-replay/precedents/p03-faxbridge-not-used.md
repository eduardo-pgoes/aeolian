---
id: p03-faxbridge-not-used
kind: correction
scope: [faxbridge]
review_by: 2026-11-01
confidence: stated
source: "fixture: a reviewer said faxbridge is \"not actually used\""
---
The faxbridge integration is still in the code but isn't used in production. Don't describe it as an active provider or put it in a current-state map.

**how to check:** a claim that faxbridge is live, active or in the request path is `contradicted`. NOTE: `review_by` is already past on purpose (2026-11-01). This fixture tests the digest's stale-precedent flag.
