#!/usr/bin/env bash
#
# Fails when a pipeline package imports one above it.
#
# The engine-structure proposal (https://github.com/ev-shindin/llm-scaler/pull/87)
# gives the dependency order of the pipeline: engine > policy > plan > analyze > signals > collect / actuate >
# decision > domain. A package may import only what is below it in that
# order, plus the infrastructure leaves (config, constants, metrics, logging,
# prometheus, accelerator, gpunodes, kueue, inferenceengine, variant,
# datastore, utils/...), which are importable from anywhere and are not
# checked here. Packages outside the listed layers (controller, warmpool,
# cmd) sit above the pipeline and are not checked either.
#
# Non-test imports only: a test may import anything.
#
# The layers name TODAY'S packages; when a package moves under the proposal
# its new path replaces the old one here, and the rule stays.
set -u
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 1

MODULE="$(sed -n 's/^module //p' go.mod)"
[ -n "$MODULE" ] || { echo "FATAL: cannot read the module path from go.mod" >&2; exit 1; }
command -v go >/dev/null 2>&1 || { echo "FATAL: go is required to run this check" >&2; exit 1; }

# Higher number = higher layer. A package may import its own layer or any
# below it; only an edge to a HIGHER number fails. Equal is deliberate --
# analyzers sit beside each other and neither is above the other, and the two
# drivers share nothing.
#
# Layers are spaced by ten so that a package CAN be ranked between two of them
# where the order is real, without renumbering the layers around it.
# Two packages are ranked that way. internal/signals/floor (31) composes the
# packages that only measure, so an edge from one of them back to it is an
# inversion, and at one shared number the check could not see it.
# internal/signals/whatif (32) prices from what they measure and sits above
# floor (docs/proposals/slo-capacity-planner.md, "Packaging"). Nothing else
# moves -- aggregation and the measuring signals packages stay equals, as the
# proposal has them.
layer_of() {
    case "$1" in
        internal/engines/steadystate|internal/engines/scalefromzero) echo 70 ;;
        internal/scalingpolicy) echo 60 ;;
        internal/engines/allocation|internal/engines/allocation/*) echo 50 ;;
        internal/engines/analyzers/*|internal/engines/executor|internal/engines/variantmeta) echo 40 ;;
        # whatif prices capacity from the lines the measuring packages fit
        # (docs/proposals/slo-capacity-planner.md, "Packaging"): the analyzers
        # call it, so it is below them, and it sits above floor so that an
        # edge from floor, or from a package that only measures, back to it
        # is an inversion. Like floor's, this arm MUST precede
        # internal/signals/* below.
        internal/signals/whatif|internal/signals/whatif/*) echo 32 ;;
        # floor reads the capacity window and prices the load from it, so it
        # is above the packages that only measure -- and above aggregation,
        # which it imports. Those keep the one rank they have always shared.
        #
        # This arm MUST precede internal/signals/* below: case takes the first
        # match and * matches a slash, so the catch-all would otherwise claim
        # floor and its subpackages and put them back at 30 -- silently, with
        # the clean tree still passing.
        internal/signals/floor|internal/signals/floor/*) echo 31 ;;
        # prefill (the what-if PR P1) only measures: it learns V_P, a and f
        # from the rows, and whatif (32) and the analyzers read it. So it
        # keeps the measuring packages' rank, named so that an edge from
        # it up to floor or whatif reads as the inversion it is.
        internal/engines/aggregation|internal/signals/prefill|internal/signals/*) echo 30 ;;
        internal/collector|internal/collector/*) echo 20 ;;
        internal/actuator|internal/scaler|internal/registry) echo 20 ;;
        internal/decision) echo 10 ;;
        internal/domain) echo 0 ;;
        *) echo "" ;;
    esac
}

# One line per package: "<path> <import> <import> ...", module prefix stripped.
listing="$(go list -f '{{.ImportPath}} {{join .Imports " "}}' ./internal/... 2>/dev/null | sed "s|$MODULE/||g")" || {
    echo "FATAL: go list failed" >&2; exit 1
}
[ -n "$listing" ] || { echo "FATAL: go list returned nothing; the check ran against no packages" >&2; exit 1; }

checked=0; bad=0
while read -r pkg imports; do
    from="$(layer_of "$pkg")"
    [ -n "$from" ] || continue
    checked=$((checked + 1))
    for imp in $imports; do
        case "$imp" in internal/*) ;; *) continue ;; esac
        to="$(layer_of "$imp")"
        [ -n "$to" ] || continue
        if [ "$to" -gt "$from" ]; then
            echo "UPWARD  $pkg (layer $from) imports $imp (layer $to)"
            bad=$((bad + 1))
        fi
    done
done <<< "$listing"

[ "$checked" -gt 0 ] || { echo "FATAL: no layered package was found; the layer table no longer matches the tree" >&2; exit 1; }
if [ "$bad" -gt 0 ]; then
    echo "$bad upward import(s) across $checked layered package(s); see the engine-structure proposal (PR #87)" >&2
    exit 1
fi
echo "import direction OK ($checked layered packages, no upward edge)"
