---
id: p11-read-only-is-not-harmless
kind: safety
scope: [prod]
review_by: 2027-05-09
confidence: stated
source: "fixture: \"everything was read-only\", yet a parallel log scan throttled a shared table"
---
Read-only prod access can still hurt prod. Parallel scans and queries throttle shared tables. Prod reads must be rate-limited and budgeted, and the queries go in the run directory (not ad hoc in /tmp) so they can be audited.

**how to check:** a claim that a prod action is safe *because* it is read-only is `contradicted`. A plan that fans out prod queries without a stated rate or budget gets flagged.
