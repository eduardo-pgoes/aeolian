---
id: p04-self-hosted-grafana-dead
kind: correction
scope: [grafana]
review_by: 2027-02-09
confidence: inferred
source: "fixture: a reviewer's summary listed the self-hosted Grafana among dead services (not a direct quote)"
---
The self-hosted Grafana is a dead service. A deployment manifest or a dashboard JSON in a repo doesn't make it live.

**how to check:** a claim that the self-hosted Grafana is the primary or current dashboard is `contradicted`. Note that this precedent is inferred, so the verdict note should say the user may want to confirm it.
