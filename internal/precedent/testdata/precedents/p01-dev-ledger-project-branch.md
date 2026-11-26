---
id: p01-dev-ledger-project-branch
kind: correction
scope: [dev-ledger, branch]
review_by: 2027-01-09
confidence: stated
source: "fixture: a reviewer said \"dev-ledger… it's a PROJECT branch\""
---
`dev-ledger` in atlas is a long-lived project branch for the ledger rewrite. Being ahead of or behind main is expected, not drift, and it must not be called abandoned or proposed for deletion. This goes stale once the project merges, so re-check it by `review_by`.

**how to check:** if a claim calls `dev-ledger` drift, stale, abandoned or deletable, mark it `contradicted` and cite this precedent. A claim that only describes its commit distance from main can still hold.
