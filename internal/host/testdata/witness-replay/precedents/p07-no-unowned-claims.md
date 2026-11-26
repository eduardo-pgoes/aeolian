---
id: p07-no-unowned-claims
kind: convention
scope: [claim]
review_by: 2027-05-09
confidence: stated
source: "fixture: a reviewer asked \"which bot token? where did you get that?\""
---
Every factual claim needs evidence the reviewer can open: a `path:line` in a checked-out repo, a command with its output, or a public URL. A claim with no evidence, or with evidence behind a login the reviewer doesn't have, is unowned.

**how to check:** if no evidence resolves and opens, the verdict is `unverifiable`, whatever the claim says. Never upgrade it to `holds` because it sounds plausible.
