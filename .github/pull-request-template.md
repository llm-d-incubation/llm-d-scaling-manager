Fixes #

<!-- Please include the 'why' behind your changes if no issue exists -->

## Proposed Changes

<!-- Please categorize your changes:
- :gift: Add new feature
- :bug: Fix bug
- :broom: Update or clean up current behavior
- :wastebasket: Remove feature or internal logic
-->

-
-
-

<!--
The section below is checked by CI, line by line. Keep its heading exactly.
If no golden moves, leave it at none.

If the PR moves a golden, replace none with one line per golden: a file under
internal/engines/analyzers/learnreplay/testdata/goldens/, a
cmd/whatif/testdata/*.golden, cmd/whatif/testdata/schema-v*.txt, or
internal/engines/steadystate/testdata/engine-run-event-counts.txt. A line is a
dash, a space, one name in backticks, and optionally a space, a dash, a space and
what moved and which way:

- `acceptance/results.csv` - decode demand up on 12 cycles, by at most 0.5 %
- `pd-restart/*.csv` - new fixture
- `pd-restart/recording.jsonl.gz` - new fixture
- `goldens.yaml` - lists it

Nothing else may sit in the section. Put prose and the replay summary under the
next heading. Editing the section re-runs the check.
The format: docs/developer-guide/testing.md, "The goldens: section".
-->
## goldens:

none

### Replay summary

<!--
For a learner golden, paste the per-output summary from make learner-goldens-check,
never the CSV. That target replays learner goldens only: for any other golden, say
what moved. Delete this heading if no golden moves.
-->

### Pre-review Checklist

<!-- If these boxes are not checked, you will be asked to complete these requirements or explain why they do not apply to your PR. -->

- [ ] **E2E tests** for any new behavior
- [ ] **Docs** updated under `docs/` for any user-facing impact
- [ ] **Proposal** under `docs/proposals/` for any new enhancement or change to existing behavior

**Release Note**

<!--
:page_facing_up: If this change has user-visible impact, write a release note in the block
below. Include the string "action required" if additional action is required of
users switching to the new release, for example in case of a breaking change.

Write as if you are speaking to users, not other contributors. If this
change has no user-visible impact, no release note is needed.
-->

```release-note

```


**Docs**

<!--
:book: If this change has user-visible impact, list the pages it updates in this
repository: docs/reference/, docs/concepts/ or docs/well-lit-paths/ for user-facing
documentation, docs/guides/ for guides (edit guide.yaml and run `make guides-render`).
-->
