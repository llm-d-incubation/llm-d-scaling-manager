#!/usr/bin/env bash
#
# Runs the capacity planner page's scenario logic on fixtures
# (hack/check-planner-scenarios.mjs on internal/whatifapi/page/scenario.js), and its day
# plan, check and first screen (check-planner-dayplan.mjs, -backtest.mjs, -answer.mjs), and
# the answer panel asked again as the knobs change (check-planner-exec.mjs), and the SLO
# target controls and the day plan marked stale when they change (check-planner-targets.mjs),
# the colour tokens against WCAG 2.1 AA (check-planner-contrast.mjs), the boxes that show
# the value the source learned (check-planner-learned.mjs), and the day plan while it runs
# (check-planner-dayprogress.mjs).
#
# The page saves scenarios in the browser and imports them from files, and
# both are untrusted when read back. The Go tests can only read the page's
# text: a check that admits everything, an escape that escapes nothing, or a
# store edit that erases another version's entries all read fine there. This
# runs the functions.
#
# It needs node (CI sets it up in the deploy-scripts-check job), and fails
# without it: a skipped check reads as a pass.
set -euo pipefail

cd "$(dirname "$0")/.." || exit 1

if ! command -v node >/dev/null 2>&1; then
  echo "FAIL: node is required (not skipping: a skipped check reads as a pass)" >&2
  exit 1
fi

node hack/check-planner-scenarios.mjs internal/whatifapi/page/scenario.js
node hack/check-planner-dayplan.mjs internal/whatifapi/page/scenario.js internal/whatifapi/page/dayplan.js \
  cmd/capacity-planner/testdata/day-plan-ols-sweep-json.golden cmd/capacity-planner/testdata/day-plan-staircase-b-on-json.golden
node hack/check-planner-backtest.mjs internal/whatifapi/page/scenario.js internal/whatifapi/page/dayplan.js internal/whatifapi/page/backtest.js \
  cmd/capacity-planner/testdata/backtest-q32-split-json.golden
node hack/check-planner-answer.mjs internal/whatifapi/page/scenario.js internal/whatifapi/page/dayplan.js internal/whatifapi/page/backtest.js \
  internal/whatifapi/page/planner.css cmd/capacity-planner/testdata/at-replicas-staircase.json.golden cmd/capacity-planner/testdata/backtest-q32-split-json.golden
node hack/check-planner-exec.mjs internal/whatifapi/page/scenario.js internal/whatifapi/page/dayplan.js internal/whatifapi/page/backtest.js \
  internal/whatifapi/page/planner.js
node hack/check-planner-targets.mjs internal/whatifapi/page/scenario.js internal/whatifapi/page/dayplan.js internal/whatifapi/page/backtest.js \
  internal/whatifapi/page/planner.js cmd/capacity-planner/testdata/day-plan-ols-sweep-json.golden internal/whatifapi/page/index.html
node hack/check-planner-contrast.mjs internal/whatifapi/page/planner.css
node hack/check-planner-learned.mjs internal/whatifapi/page/scenario.js internal/whatifapi/page/dayplan.js internal/whatifapi/page/backtest.js \
  internal/whatifapi/page/planner.js internal/whatifapi/page/index.html
node hack/check-planner-dayprogress.mjs internal/whatifapi/page/scenario.js internal/whatifapi/page/dayplan.js internal/whatifapi/page/backtest.js \
  internal/whatifapi/page/planner.js cmd/capacity-planner/testdata/day-plan-ols-sweep-json.golden
