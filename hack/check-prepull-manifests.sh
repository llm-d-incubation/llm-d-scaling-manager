#!/usr/bin/env bash
# Executes deploy/prepull.sh and hack/benchmark/engine_image.sh against
# fixtures and asserts what they do.
#
# Every defect this covers parses fine: a holder that requests an accelerator
# (it would then HOLD one, the opposite of the point), a placement that lands
# on every node or on none, two images colliding on one DaemonSet name, a pull
# policy that re-pulls a pinned tag on every restart, a holder handed a
# ServiceAccount token it never uses or every GPU on the node through the
# image's own NVIDIA_VISIBLE_DEVICES, a `status` that reads a pulled image as
# absent or an evicted holder as present, a comma list of images that reaches
# the script as one argument, an engine image read from the wrong key, or a
# scenario whose engine container quietly went back to Always.
#
# `status`, `delete` and the standup's pre-pull step are exercised offline: a
# stub kubectl on PATH answers `get nodes`, `get pods`, `get daemonset`,
# `get events`, `create namespace` and `apply` from JSON fixtures written
# below, the pattern hack/check-limiter-declaration.sh uses. A live cluster is
# not needed to know what the classification does with a RunContainerError or
# an Evicted holder; the fixtures are what a real cluster answered.
set -euo pipefail

# jq is not optional: `status` reads the node and pod lists with it, and
# without it every status assertion fails with nothing saying why.
command -v jq >/dev/null 2>&1 || {
    printf 'FATAL: jq is required to run these checks.\n' >&2
    exit 1
}
cd "$(dirname "$0")/.."

PY=${PYTHON:-python3}
T="$(mktemp -d)"
trap 'rm -rf "$T"' EXIT
FAILED=0
ok()   { printf '  ok   %s\n' "$1"; }
fail() { printf '  FAIL %s\n' "$1"; FAILED=1; }

# ---------------------------------------------------------------------------
# 1. The rendered DaemonSets.
# ---------------------------------------------------------------------------
LONG='registry.example.com:5000/Some-Org/A-Very-Long-Engine-Image-Name-That-Goes-On:V1.2.3-and-a-long-tag-suffix-too'
bash deploy/prepull.sh apply -n check-ns \
    --image docker.io/vllm/vllm-openai:v0.26.0 \
    --image ghcr.io/other/vllm-openai:v0.26.0 \
    --image 'docker.io/vllm/vllm-openai@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef' \
    --image "$LONG" \
    --node-selector example.com/accelerator=h200 \
    --toleration example.com/dedicated \
    --dry-run > "$T/render.yaml"
# and once with the defaults: one toleration, the affinity over the product keys
bash deploy/prepull.sh apply -n check-ns --image docker.io/vllm/vllm-openai:v0.26.0 --dry-run > "$T/render-default.yaml"

"$PY" - "$T/render.yaml" "$T/render-default.yaml" "$LONG" deploy/lib/accelerator_labels.py <<'PYEOF' || FAILED=1
import re, sys, yaml

docs = [d for d in yaml.safe_load_all(open(sys.argv[1], encoding="utf-8")) if d]
default = [d for d in yaml.safe_load_all(open(sys.argv[2], encoding="utf-8")) if d]
long_ref = sys.argv[3]
# the product keys the rest of the repo resolves accelerator nodes through
ns = {}
exec(open(sys.argv[4], encoding="utf-8").read(), ns)
product_keys = ns["PRODUCT_KEYS"]
bad = []
def check(cond, msg):
    if not cond:
        bad.append(msg)

check(len(docs) == 4, "expected 4 DaemonSets, got %d" % len(docs))
names = [d["metadata"]["name"] for d in docs]
check(len(set(names)) == 4, "names collide: %s" % names)
label = re.compile(r"^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$")
for n in names:
    check(bool(label.match(n)), "not a DNS-1123 label (<=63, lowercase, no leading/trailing hyphen): %r" % n)
check("very-long-engine-image-name-that-goes" in names[3],
      "the long mixed-case reference is case-folded into its name, not scrubbed: %r" % names[3])
images = ["docker.io/vllm/vllm-openai:v0.26.0", "ghcr.io/other/vllm-openai:v0.26.0",
          "docker.io/vllm/vllm-openai@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", long_ref]
for d, image in zip(docs, images):
    check(d["kind"] == "DaemonSet", "kind %s" % d["kind"])
    check(d["metadata"]["namespace"] == "check-ns", "namespace")
    ann = d["metadata"]["annotations"]
    check(ann["scaling-manager.llmd.ai/prepull-image"] == image, "annotation names the image: %s" % ann)
    check(ann["scaling-manager.llmd.ai/prepull-node-selector"] == "example.com/accelerator=h200", "annotation records the selector: %s" % ann)
    tpl = d["spec"]["template"]
    ps = tpl["spec"]
    check(ps["nodeSelector"] == {"example.com/accelerator": "h200"}, "nodeSelector: %s" % ps["nodeSelector"])
    check("affinity" not in ps, "an explicit selector replaces the affinity, not adds to it")
    keys = {t["key"] for t in ps["tolerations"]}
    check({"nvidia.com/gpu", "example.com/dedicated"} <= keys, "tolerations: %s" % keys)
    check(ps.get("automountServiceAccountToken") is False, "the holder must not mount a ServiceAccount token")
    check((ps.get("securityContext") or {}).get("seccompProfile", {}).get("type") == "RuntimeDefault", "pod seccompProfile RuntimeDefault")
    sel = d["spec"]["selector"]["matchLabels"]
    check(all(tpl["metadata"]["labels"].get(k) == v for k, v in sel.items()), "selector does not match the template labels")
    check(tpl["metadata"]["labels"].get("app.kubernetes.io/managed-by") == "scaling-manager-prepull", "the pods carry managed-by, so the pod list is scoped like the DaemonSet list")
    cs = ps["containers"]
    check(len(cs) == 1, "one container")
    c = cs[0]
    check(c["image"] == image, "container image %s" % c["image"])
    check(c["imagePullPolicy"] == "IfNotPresent", "pull policy %s" % c["imagePullPolicy"])
    res = c.get("resources", {})
    for section in ("requests", "limits"):
        for k in res.get(section, {}):
            check("gpu" not in k and "/" not in k, "the holder requests an accelerator: %s" % k)
    check(res.get("limits", {}).get("memory"), "a memory limit, so a holder can never grow")
    env = {e["name"]: e.get("value") for e in c.get("env", [])}
    check(env.get("NVIDIA_VISIBLE_DEVICES") == "void",
          "NVIDIA_VISIBLE_DEVICES must be void: engine images bake in =all and the runtime injects every GPU into a container that requested none")
    check(c["command"][0] == "/bin/sh", "runs the image itself, asleep")
    check("$!" in c["command"][2] and "\\$" not in c["command"][2], "the sleep loop's $! reaches the pod unescaped")
    csc = c.get("securityContext", {})
    check(csc.get("allowPrivilegeEscalation") is False and "ALL" in csc.get("capabilities", {}).get("drop", []), "container drops all capabilities")
    check(csc.get("readOnlyRootFilesystem") is True, "the holder writes nothing; its root filesystem is read-only")
    check("terminationGracePeriodSeconds" in ps, "short termination so a holder yields its node quickly")

check(len(default) == 1, "one DaemonSet with defaults")
if default:
    d = default[0]
    ps = d["spec"]["template"]["spec"]
    check(ps["tolerations"] == [{"key": "nvidia.com/gpu", "operator": "Exists"}], "with no --toleration exactly nvidia.com/gpu is tolerated: %s" % ps["tolerations"])
    check("nodeSelector" not in ps, "no --node-selector: no nodeSelector (it cannot express OR)")
    terms = ps.get("affinity", {}).get("nodeAffinity", {}).get("requiredDuringSchedulingIgnoredDuringExecution", {}).get("nodeSelectorTerms", [])
    keys = []
    for t in terms:
        exprs = t.get("matchExpressions", [])
        ops = [(e.get("key"), e.get("operator"), e.get("values")) for e in exprs]
        check(len(exprs) == 2 and ops[0][1] == "Exists" and ops[1] == (ops[0][0], "NotIn", [""]),
              "each term is one key: Exists AND NotIn [empty] (an empty product label is a CPU node): %s" % t)
        keys.append(ops[0][0])
    check(keys == product_keys, "the default affinity is one term per product key, in order: %s vs %s" % (keys, product_keys))
    check(d["metadata"]["annotations"]["scaling-manager.llmd.ai/prepull-node-selector"] == "", "the default records an empty selector")

if bad:
    for b in bad:
        print("  FAIL " + b)
    sys.exit(1)
print("  ok   %d DaemonSets rendered: names, placement (selector or product-key affinity), tolerations, no accelerator, no SA token, no GPU injection, seccomp, IfNotPresent" % len(docs))
PYEOF

selbad=0
for badsel in notakeyvalue 'a=b"' '=v' 'a b=c' 'a=b,' ',a=b' 'a=b,,c=d' 'a=b,a=c' 'a=b c' 'a=b, c=d' 'a=b,c' 'a!=b'; do
    if bash deploy/prepull.sh apply -n check-ns --image a:1 --node-selector "$badsel" --dry-run >/dev/null 2>&1; then
        fail "a --node-selector of '$badsel' must be refused"; selbad=1
    fi
done
[ "$selbad" -eq 0 ] && ok "a --node-selector that is not KEY=VALUE pairs of label characters, one comma apart, each key once, is refused"
# several pairs, comma separated: a node must carry every one, so each is a
# nodeSelector entry; the annotation records the selector as given
SEL2=example.com/accelerator=h200,example.com/state=production
if bash deploy/prepull.sh apply -n check-ns --image a:1 --node-selector "$SEL2" --dry-run > "$T/render-sel2.yaml" 2>&1; then
    [ "$(yq -o json -I0 'select(.kind == "DaemonSet") | .spec.template.spec.nodeSelector' "$T/render-sel2.yaml")" = '{"example.com/accelerator":"h200","example.com/state":"production"}' ] \
        && [ "$(yq -r 'select(.kind == "DaemonSet") | .metadata.annotations["scaling-manager.llmd.ai/prepull-node-selector"]' "$T/render-sel2.yaml")" = "$SEL2" ] \
        && ok "two pairs: the nodeSelector carries both keys, the annotation the selector as given" || fail "two pairs rendered: $(yq -o json -I0 '[.metadata.annotations, .spec.template.spec.nodeSelector]' "$T/render-sel2.yaml")"
else
    fail "a two-pair --node-selector was refused: $(tail -1 "$T/render-sel2.yaml")"
fi
"$PY" -c 'import re, sys; sys.exit(0 if re.search(r"\n      nodeSelector:\n        \"example\.com/accelerator\": \"h200\"\n(?!        )", open(sys.argv[1]).read()) else 1)' "$T/render.yaml" \
    && ok "one pair: the nodeSelector block is the one it always was, the key now quoted as the value is" || fail "one pair: the nodeSelector block changed"

# A newline is not a pair separator: the pairs are read a line at a time, so
# one inside a pair used to split it into two the nodeSelector rendered and
# `kubectl get nodes -l` never saw. A carriage return and a tab go with it.
selbad=0
for badsel in $'a=b\nc=d' $'a=b\r' $'a=b\tc=d' $'a\n=b'; do
    if bash deploy/prepull.sh apply -n check-ns --image a:1 --node-selector "$badsel" --dry-run >/dev/null 2>&1; then
        fail "a --node-selector with a newline, CR or tab must be refused: $(printf '%q' "$badsel")"; selbad=1
    fi
done
[ "$selbad" -eq 0 ] && ok "a --node-selector holding a newline, carriage return or tab is refused"
# Kubernetes label syntax, not just label characters: a key is an optional
# DNS-subdomain prefix (<= 253, lowercase) and '/', then a name (<= 63); a
# value is empty or follows the name rule; names and values start and end
# with a letter or digit.
long64="$(printf 'k%.0s' $(seq 64))"
long63="$(printf 'k%.0s' $(seq 63))"
prefix254="$(printf 'p%.0s' $(seq 250)).com"
selbad=0
for badsel in 'a/b/c=v' '/x=v' 'x/=v' '-a=v' 'a-=v' '_a=v' 'a=-b' 'a=b-' 'a=.b' 'a=b=c' "${long64}=v" "k=${long64}" "ex.com/${long64}=v" "${prefix254}/k=v" 'Example.com/k=v' 'ex_ample.com/k=v' '-ex.com/k=v' 'ex.com-/k=v' 'a..b/k=v' 'a.-b/k=v' 'a-.b/k=v' '-a/k=v' 'a-/k=v' '.a/k=v' 'a./k=v'; do
    if bash deploy/prepull.sh apply -n check-ns --image a:1 --node-selector "$badsel" --dry-run >/dev/null 2>&1; then
        fail "a --node-selector outside label syntax must be refused: '$badsel'"; selbad=1
    fi
done
[ "$selbad" -eq 0 ] && ok "a key or value outside Kubernetes label syntax is refused (two slashes, an empty or bad prefix, a name or value over 63, a bad first or last character, a prefix with an empty or dash-edged dot-separated part)"
selgood=0
for goodsel in 'a=b' 'a=' 'gpu.nvidia.com/model=' 'example.com/x_y.z=A-1_b.2' "${long63}=${long63}" "ex.com/${long63}=v" 'k8s.io/a=b,c=' 'A=b' 'a.b-c_d=0' 'a.b-c/k=v' '10.0.0.1/k=v' 'kubernetes.io/k=v' 'a--b.c/k=v'; do
    if ! bash deploy/prepull.sh apply -n check-ns --image a:1 --node-selector "$goodsel" --dry-run > "$T/render-good.yaml" 2>&1; then
        fail "a --node-selector in label syntax must be accepted: '$goodsel': $(tail -1 "$T/render-good.yaml")"; selgood=1
    fi
done
[ "$selgood" -eq 0 ] && ok "keys and values in label syntax are accepted, an empty value included (63-character names and values, a prefixed key)"
# an empty value is a value: it selects the nodes whose label is empty, and
# the nodeSelector carries it as ""
bash deploy/prepull.sh apply -n check-ns --image a:1 --node-selector 'gpu.nvidia.com/model=' --dry-run > "$T/render-empty.yaml"
[ "$(yq -o json -I0 'select(.kind == "DaemonSet") | .spec.template.spec.nodeSelector' "$T/render-empty.yaml")" = '{"gpu.nvidia.com/model":""}' ] \
    && ok "an empty value renders as an empty-string nodeSelector entry" || fail "empty value rendered: $(yq -o json -I0 'select(.kind == "DaemonSet") | .spec.template.spec.nodeSelector' "$T/render-empty.yaml")"
# Keys are quoted as values are. Unquoted, a key such as on, yes or true is a
# boolean to a YAML 1.1 reader (PyYAML here), and a nodeSelector key must be
# a string; yq (YAML 1.2) must read the same map.
bash deploy/prepull.sh apply -n check-ns --image a:1 --node-selector 'on=yes,true=1,no=off' --dry-run > "$T/render-boolkeys.yaml"
"$PY" -c '
import sys, yaml
ds = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d and d["kind"] == "DaemonSet"]
ns = [d["spec"]["template"]["spec"]["nodeSelector"] for d in ds]
want = {"on": "yes", "true": "1", "no": "off"}
sys.exit(0 if ns and all(n == want and all(isinstance(k, str) for k in n) for n in ns) else "YAML 1.1 read %s" % ns)
' "$T/render-boolkeys.yaml" && ok "keys such as on, true and no stay strings under a YAML 1.1 reader" || fail "keys read as booleans by a YAML 1.1 reader"
[ "$(yq -o json -I0 'select(.kind == "DaemonSet") | .spec.template.spec.nodeSelector' "$T/render-boolkeys.yaml" | sort -u)" = '{"on":"yes","true":"1","no":"off"}' ] \
    && [ "$(yq -r 'select(.kind == "DaemonSet") | .spec.template.spec.nodeSelector | keys | map(type) | unique | join(",")' "$T/render-boolkeys.yaml" | sort -u)" = '!!str' ] \
    && ok "and yq reads the same map, string keys" || fail "yq read: $(yq -o json -I0 'select(.kind == "DaemonSet") | .spec.template.spec.nodeSelector' "$T/render-boolkeys.yaml")"
# A live nodeSelector whose key holds a comma cannot be read back as pairs.
# What is printed instead names no key, so no comma in a key splits it, and
# it is refused whole.
sentinel="$(printf '%s' '{"a,b":"c","d":"e"}' | bash -c '. deploy/lib/accelerator_nodes.sh; accelerator_selector_from_json')"
why="$(bash -c '. deploy/lib/accelerator_nodes.sh; accelerator_selector_ok "$1"' _ "$sentinel")" && fail "the unreadable-nodeSelector string was accepted: '$sentinel'" || {
    case "$sentinel" in
        *,*) fail "the unreadable-nodeSelector string holds a comma, so it is split: '$sentinel' -> $why" ;;
        "unreadable nodeSelector"*) ok "a live nodeSelector with a comma in a key reads as one fixed string, refused whole: $why" ;;
        *) fail "the unreadable-nodeSelector string: '$sentinel'" ;;
    esac
}
if bash deploy/prepull.sh status -n check-ns --dry-run >/dev/null 2>&1; then fail "status must refuse --dry-run"; else ok "status: --dry-run is refused (an apply/delete option)"; fi
if bash deploy/prepull.sh apply -n check-ns --image a:1 --toleration '' --dry-run >/dev/null 2>&1; then
    fail "an empty --toleration (which tolerates every taint) must be refused"
else
    ok "an empty --toleration is refused"
fi

# ---------------------------------------------------------------------------
# 2. engine_image.sh: the harness's own pin, or nothing.
# ---------------------------------------------------------------------------
mkdir -p "$T/clone/config/templates/values"
cat > "$T/clone/config/templates/values/defaults.yaml" <<'EOF'
versions:
  vllm-openai_version: &vllm-openai_version v0.26.0
images:
  vllm:
    repository: docker.io/vllm/vllm-openai
    tag: *vllm-openai_version
EOF
# `|| true` on the assignments: a failing command substitution in a plain
# assignment ends the script under set -e, and the FAIL line it feeds would
# never print.
got="$(bash hack/benchmark/engine_image.sh "$T/clone" || true)"
if [ "$got" = "docker.io/vllm/vllm-openai:v0.26.0" ]; then ok "engine_image.sh reads repository:tag with the anchor resolved"; else fail "engine_image.sh printed '$got'"; fi
if out="$(bash hack/benchmark/engine_image.sh "$T/nonexistent" 2>/dev/null)"; then fail "engine_image.sh must fail on a missing clone"; else
    [ -z "$out" ] && ok "engine_image.sh prints nothing and fails on a missing clone" || fail "engine_image.sh printed on failure: '$out'"; fi
mkdir -p "$T/clone2/config/templates/values"
printf 'images:\n  vllm:\n    repository: docker.io/vllm/vllm-openai\n' > "$T/clone2/config/templates/values/defaults.yaml"
if bash hack/benchmark/engine_image.sh "$T/clone2" >/dev/null 2>&1; then fail "engine_image.sh must fail without a tag"; else ok "engine_image.sh fails without a tag rather than guessing"; fi
# A scenario's images.vllm overrides the default field by field: the prepull
# and the engine-cache preparer must hold the image the engines will run, not
# the harness default the scenario moved away from.
cat > "$T/sc-shared.yaml" <<'EOF'
shared:
  images:
    vllm:
      tag: v0.31.0
scenario:
  - name: a
  - name: b
EOF
got="$(bash hack/benchmark/engine_image.sh "$T/clone" "$T/sc-shared.yaml" || true)"
if [ "$got" = "docker.io/vllm/vllm-openai:v0.31.0" ]; then ok "a shared images.vllm.tag overrides the default tag, repository kept"; else fail "shared override: engine_image.sh printed '$got'"; fi
printf 'shared:\n  monitoring: {}\nscenario:\n  - name: a\n' > "$T/sc-none.yaml"
got="$(bash hack/benchmark/engine_image.sh "$T/clone" "$T/sc-none.yaml" || true)"
if [ "$got" = "docker.io/vllm/vllm-openai:v0.26.0" ]; then ok "a scenario without images.vllm keeps the default"; else fail "no override: engine_image.sh printed '$got'"; fi
cat > "$T/sc-stack.yaml" <<'EOF'
shared:
  images:
    vllm:
      tag: v0.31.0
scenario:
  - name: a
  - name: b
    images:
      vllm:
        repository: quay.io/other/vllm
EOF
got="$(bash hack/benchmark/engine_image.sh "$T/clone" "$T/sc-stack.yaml" || true)"
if [ "$got" = "docker.io/vllm/vllm-openai:v0.31.0,quay.io/other/vllm:v0.31.0" ]; then ok "a stack's own override layers on the shared one; distinct images print comma-separated"; else fail "stack override: engine_image.sh printed '$got'"; fi
if out="$(bash hack/benchmark/engine_image.sh "$T/clone" "$T/no-such-scenario.yaml" 2>/dev/null)"; then fail "engine_image.sh must fail on a named scenario that is missing"; else
    [ -z "$out" ] && ok "engine_image.sh prints nothing and fails on a missing scenario" || fail "engine_image.sh printed on a missing scenario: '$out'"; fi
# The P/D share scenario moved off the harness default (NIXL on v0.26.0 drops
# the KV link); what it pins is what the standup must hold on the nodes.
got="$(bash hack/benchmark/engine_image.sh "$T/clone" hack/benchmark/scenarios/guides/two-model-shapes-pd.yaml || true)"
if [ "$got" = "docker.io/vllm/vllm-openai:v0.31.0" ]; then ok "two-model-shapes-pd resolves to the engine it pins"; else fail "two-model-shapes-pd: engine_image.sh printed '$got'"; fi

# ---------------------------------------------------------------------------
# 3. status, delete and apply, offline, against what a cluster answered.
#
# A stub kubectl on PATH (the pattern hack/check-limiter-declaration.sh and
# hack/check-two-model-scenario.sh use) answers from the fixtures, and it
# checks the arguments it is given: a call that dropped -n or -l would still
# hit the right branch of a stub keyed on the verb alone, and the offline
# check would pass while the real call read the wrong scope. What it records
# in $T/calls is asserted below for delete and for the standup step.
# ---------------------------------------------------------------------------
IMG=docker.io/vllm/vllm-openai:v0.26.0
LBL='app.kubernetes.io/component=image-prepull,app.kubernetes.io/managed-by=scaling-manager-prepull'
bash deploy/prepull.sh apply -n check-ns --image "$IMG" --dry-run > "$T/one.yaml"
DS="$(grep -m1 '^  name:' "$T/one.yaml" | awk '{print $2}' || true)"
[ -n "$DS" ] || fail "no DaemonSet name in the rendered manifest"
# Nodes carry product labels the way real clusters do: GFD on most, the
# CoreWeave key on one, and one CPU node with none, which the default
# placement must leave out of the count.
cat > "$T/nodes.json" <<EOF
{"items":[
 {"metadata":{"name":"node-running",  "labels":{"nvidia.com/gpu.product":"H200"}}, "status":{"images":[]}},
 {"metadata":{"name":"node-listed",   "labels":{"nvidia.com/gpu.product":"H200"}}, "status":{"images":[{"names":["docker.io/vllm/vllm-openai@sha256:abc","${IMG}"]}]}},
 {"metadata":{"name":"node-pulled",   "labels":{"gpu.nvidia.com/model":"H200"}},   "status":{"images":[]}},
 {"metadata":{"name":"node-crashloop","labels":{"nvidia.com/gpu.product":"H200"}}, "status":{"images":[]}},
 {"metadata":{"name":"node-completed","labels":{"nvidia.com/gpu.product":"H200"}}, "status":{"images":[]}},
 {"metadata":{"name":"node-evicted",  "labels":{"nvidia.com/gpu.product":"H200"}}, "status":{"images":[]}},
 {"metadata":{"name":"node-nopod",    "labels":{"nvidia.com/gpu.product":"H200"}}, "status":{"images":[{"names":["something/else:1"]}]}},
 {"metadata":{"name":"node-noimages", "labels":{"nvidia.com/gpu.product":"H200"}}},
 {"metadata":{"name":"node-cpu",      "labels":{"kubernetes.io/os":"linux"}},       "status":{"images":[{"names":["${IMG}"]}]}},
 {"metadata":{"name":"node-cpu-cw",   "labels":{"gpu.nvidia.com/model":"","gpu.nvidia.com/class":""}}, "status":{"images":[]}},
 {"metadata":{"name":"node-amd",      "labels":{"amd.com/gpu.product-name":"MI300X"}}, "status":{"images":[]}},
 {"metadata":{"name":"node-cordoned", "labels":{"nvidia.com/gpu.product":"H200"}}, "spec":{"unschedulable":true,"taints":[{"key":"node.kubernetes.io/unschedulable","effect":"NoSchedule"},{"key":"node.kubernetes.io/disk-pressure","effect":"NoSchedule"}]}, "status":{"images":[]}},
 {"metadata":{"name":"node-foreign",  "labels":{"nvidia.com/gpu.product":"H200"}}, "spec":{"taints":[{"key":"example.com/team-x","value":"only","effect":"NoSchedule"}]}, "status":{"images":[]}}
]}
EOF
# the DaemonSet as apply wrote it: it tolerates nvidia.com/gpu, nothing else
printf '%s' '{"spec":{"template":{"spec":{"tolerations":[{"key":"nvidia.com/gpu","operator":"Exists"}]}}}}' > "$T/ds.json"
cat > "$T/pods.json" <<EOF
{"items":[
 {"metadata":{"labels":{"scaling-manager.llmd.ai/prepull":"${DS}"}},"spec":{"nodeName":"node-running"},"status":{"phase":"Running","containerStatuses":[{"ready":true,"state":{"running":{}}}]}},
 {"metadata":{"labels":{"scaling-manager.llmd.ai/prepull":"${DS}"}},"spec":{"nodeName":"node-listed"},"status":{"phase":"Pending","containerStatuses":[{"ready":false,"state":{"waiting":{"reason":"ContainerCreating"}}}]}},
 {"metadata":{"labels":{"scaling-manager.llmd.ai/prepull":"${DS}"}},"spec":{"nodeName":"node-pulled"},"status":{"phase":"Running","containerStatuses":[{"ready":false,"state":{"waiting":{"reason":"RunContainerError"}}}]}},
 {"metadata":{"labels":{"scaling-manager.llmd.ai/prepull":"${DS}"}},"spec":{"nodeName":"node-crashloop"},"status":{"phase":"Running","containerStatuses":[{"ready":false,"state":{"waiting":{"reason":"CrashLoopBackOff"}}}]}},
 {"metadata":{"labels":{"scaling-manager.llmd.ai/prepull":"${DS}"}},"spec":{"nodeName":"node-completed"},"status":{"phase":"Succeeded","containerStatuses":[{"ready":false,"state":{"terminated":{"reason":"Completed"}}}]}},
 {"metadata":{"labels":{"scaling-manager.llmd.ai/prepull":"${DS}"}},"spec":{"nodeName":"node-evicted"},"status":{"phase":"Failed","reason":"Evicted","message":"The node had condition: [DiskPressure]."}},
 {"metadata":{"labels":{"scaling-manager.llmd.ai/prepull":"other-ds"}},"spec":{"nodeName":"node-nopod"},"status":{"phase":"Running","containerStatuses":[{"ready":true,"state":{"running":{}}}]}}
]}
EOF
mkdir -p "$T/bin"
cat > "$T/bin/kubectl" <<EOF
#!/usr/bin/env bash
# stub: the reads and writes prepull.sh and the standup step make, answered
# from fixtures; every call is recorded and its scope arguments checked.
# The benchmark recipes put --request-timeout first; it is recorded apart and
# dropped, so the dispatch below reads the verb.
case "\$1" in --request-timeout=*) printf '%s\n' "\$1" >> "$T/timeouts"; shift ;; esac
printf '%s\n' "\$*" >> "$T/calls"
ARGV=" \$* "   # the call's own argv; inside want/refuse \$* would be THEIR arguments
want() {  # the argv must contain this exact argument pair
    case "\$ARGV" in *" \$1 \$2 "*) ;; *) echo "stub kubectl: \$1 \$2 missing from:\$ARGV" >&2; exit 2 ;; esac
}
refuse() {  # the argv must NOT contain this argument
    case "\$ARGV" in *" \$1 "*) echo "stub kubectl: unexpected \$1 in:\$ARGV" >&2; exit 2 ;; esac
}
case "\$1 \$2" in
  "get nodes")
      if [ -n "\${STUB_SELECTOR:-}" ]; then want -l "\$STUB_SELECTOR"; else refuse -l; fi
      [ -n "\${STUB_NODES_FORBIDDEN:-}" ] && { echo 'Error from server (Forbidden): nodes is forbidden' >&2; exit 1; }
      [ -n "\${STUB_NODES_ERROR:-}" ] && { echo "\$STUB_NODES_ERROR" >&2; exit 1; }
      if [ -n "\${STUB_NODES_FILE:-}" ]; then jq --arg s "\${STUB_SELECTOR:-}" '.items |= map(select((.metadata.labels // {}) as \$l | all(\$s | split(",")[]; index("=") as \$i | \$l[.[:\$i]] == .[\$i+1:])))' "\$STUB_NODES_FILE"; else cat "$T/nodes.json"; fi ;;
  "get pods")      want -n check-ns; want -l "$LBL"; cat "$T/pods.json" ;;
  "get daemonset")
      want -n check-ns
      case " \$* " in
        *" -l "*) want -l "$LBL"; printf '%s\n' "\${STUB_DAEMONSETS-${IMG}}" ;;
        *" -o json "*) cat "$T/ds.json" ;;
        *) want -o "jsonpath={.spec.template.spec.nodeSelector}"
           case "\$ARGV" in *" ${DS} "*|*" \${STUB_DS_NAME:-${DS}} "*) ;; *) echo "stub kubectl: get daemonset for a name that is not the DaemonSet:\$ARGV" >&2; exit 2 ;; esac
           if [ -n "\${STUB_DS_SELECTOR:-}" ]; then printf '%s' "\$STUB_DS_SELECTOR" | jq -Rc 'split(",") | map(index("=") as \$i | {key: .[:\$i], value: .[\$i+1:]}) | from_entries'; else printf '{}'; fi ;;
      esac ;;
  "get events")    want -n check-ns; printf '%s' "\${STUB_EVENT:-}" ;;
  "apply -n")      want -n check-ns; cat >/dev/null; echo "daemonset.apps/x configured" ;;
  "apply -f")      cat >/dev/null; echo "namespace/x configured" ;;
  "create namespace") echo "apiVersion: v1"; echo "kind: Namespace" ;;
  "delete daemonset") want -n check-ns; echo "daemonset.apps deleted" ;;
  *) echo "stub kubectl: unexpected \$*" >&2; exit 2 ;;
esac
EOF
chmod +x "$T/bin/kubectl"
STUB_PATH="$T/bin:$PATH"

set +e
PATH="$STUB_PATH" bash deploy/prepull.sh status -n check-ns > "$T/status.out" 2>&1
rc=$?
set -e
expect_line() {  # node, IMAGE column
    if grep -qE "^  $1 +$2 " "$T/status.out"; then ok "status: $1 -> $2"; else fail "status: $1 expected $2; got: $(grep -E "^  $1 " "$T/status.out" || echo none)"; fi
}
expect_line node-running   present
expect_line node-listed    present
expect_line node-pulled    pulled
expect_line node-crashloop pulled
expect_line node-completed pulled
expect_line node-evicted   absent
expect_line node-nopod     absent
expect_line node-noimages  absent
grep -q '^  node-cpu ' "$T/status.out" && fail "status: a node with no product label was counted under the default placement" || ok "status: the default placement leaves a node with no product label out"
grep -q '^  node-cpu-cw ' "$T/status.out" && fail "status: a node whose product labels are EMPTY (a CoreWeave CPU node) was counted" || ok "status: the default placement leaves a node with empty product labels out"
grep -q 'node-evicted .*Evicted' "$T/status.out" && ok "status: the evicted holder shows its reason" || fail "status: Evicted reason missing"
grep -q 'node-pulled .*not held' "$T/status.out" && ok "status: a pulled-but-unheld image says so" || fail "status: pulled/not-held note missing"
grep -q '5/9 usable nodes have it (2 held, 3 pulled but not held); 4 do not' "$T/status.out" && ok "status: the tally separates held from pulled-not-held, on usable nodes" || fail "status: tally wrong: $(grep 'nodes have' "$T/status.out")"
grep -qE '^  node-cordoned +excluded +cordoned' "$T/status.out" && grep -qE '^  node-foreign +excluded +tainted example.com/team-x:NoSchedule' "$T/status.out" && ok "status: a cordoned node and a node tainted against the holder are listed as excluded, with why" || fail "status: excluded rows: $(grep -E 'node-cordoned|node-foreign' "$T/status.out" | tr '\n' ';')"
# A complete hold exits 0 beside a cordoned node and a foreign-tainted one:
# counted, they reported 8/17 on a finished preparation and failed it.
cp "$T/nodes.json" "$T/nodes-full.json"
jq '.items |= map(select(.metadata.name == "node-running" or .metadata.name == "node-cordoned" or .metadata.name == "node-foreign"))' "$T/nodes-full.json" > "$T/nodes.json"
if PATH="$STUB_PATH" bash deploy/prepull.sh status -n check-ns > "$T/done.out" 2>&1; then grep -q '1/1 usable nodes have it' "$T/done.out" && ok "status: every usable node holding the image is a pass, unusable nodes aside" || fail "status complete: $(grep 'nodes have' "$T/done.out")"; else fail "status: a complete hold failed on nodes no engine can use: $(grep -E 'nodes have|excluded' "$T/done.out" | tr '\n' ';')"; fi
cp "$T/nodes-full.json" "$T/nodes.json"
grep -q '1 node(s) carry an AMD, Intel or Gaudi accelerator label' "$T/status.out" && ok "status: under the default placement a non-NVIDIA node is named as held for nothing" || fail "status: vendor warning missing"
grep -q 'carry an AMD' "$T/sel.out" 2>/dev/null && fail "status: the vendor warning must not print under an explicit selector" || true
[ "$rc" -ne 0 ] && ok "status: exits non-zero while a node lacks the image" || fail "status: exit 0 with nodes lacking the image"
grep -q "^${IMG}  (nodes: any node carrying" "$T/status.out" && ok "status: with no --image the held images are discovered from the DaemonSets, with the recorded placement" || fail "status: discovery from DaemonSets failed: $(head -1 "$T/status.out")"
grep -q 'stub kubectl:' "$T/status.out" && fail "status: a kubectl call lacked its scope arguments: $(grep 'stub kubectl:' "$T/status.out" | head -1)" || ok "status: every kubectl call carried -n / -l, and none an -l for nodes under the default placement"

# the DaemonSet's recorded selector is what status lists nodes with; the flag overrides
: > "$T/calls"
STUB_DS_SELECTOR=example.com/accelerator=h200 STUB_SELECTOR=example.com/accelerator=h200 PATH="$STUB_PATH" bash deploy/prepull.sh status -n check-ns > "$T/recorded.out" 2>&1 || true
grep -q 'get nodes -l example.com/accelerator=h200' "$T/calls" && ok "status: lists the nodes the DaemonSet was applied for (its live nodeSelector, so a DaemonSet from before this script counts too)" || fail "status: live selector not used: $(grep 'get nodes' "$T/calls")"
: > "$T/calls"
STUB_DS_SELECTOR=example.com/accelerator=h200 STUB_SELECTOR=other/key=v PATH="$STUB_PATH" bash deploy/prepull.sh status -n check-ns --image "$IMG" --node-selector other/key=v >/dev/null 2>&1 || true
grep -q 'get nodes -l other/key=v' "$T/calls" && ok "status: --node-selector overrides the recorded one" || fail "status: override not passed: $(grep 'get nodes' "$T/calls")"
STUB_SELECTOR=example.com/accelerator=h200 PATH="$STUB_PATH" bash deploy/prepull.sh status -n check-ns --image "$IMG" --node-selector example.com/accelerator=h200 > "$T/sel.out" 2>&1 || true
grep -q "^${IMG}  (nodes: example.com/accelerator=h200)" "$T/sel.out" && ok "status: an explicit selector is named in the per-image header" || fail "status: selector text: $(head -1 "$T/sel.out")"
# a DaemonSet whose live nodeSelector is not one this script writes (a tenant edited it)
if STUB_DS_SELECTOR='a=b"c' PATH="$STUB_PATH" bash deploy/prepull.sh status -n check-ns --image "$IMG" > "$T/edited.out" 2>&1; then fail "status must refuse a DaemonSet nodeSelector that is not label characters"; else
    grep -q 'this script did not write' "$T/edited.out" && ok "status: a DaemonSet nodeSelector this script did not write is refused before it reaches kubectl" || fail "status on an edited DaemonSet: $(tail -1 "$T/edited.out")"; fi
: > "$T/calls"
STUB_DS_SELECTOR='a=b"c' PATH="$STUB_PATH" bash deploy/prepull.sh apply -n check-ns --image "$IMG" > "$T/edited-apply.out" 2>&1 || true
grep -q 'get nodes' "$T/calls" && fail "apply's report passed an unvalidated nodeSelector to kubectl" || ok "apply: the report warns on an edited nodeSelector and never lists nodes with it"
# nodes cannot be listed for a reason that is not a Forbidden: that reason, not the permission story
if STUB_NODES_ERROR='error: unable to parse requirement' PATH="$STUB_PATH" bash deploy/prepull.sh status -n check-ns --image "$IMG" > "$T/notforbidden.out" 2>&1; then fail "status must fail when nodes cannot be listed"; else
    grep -q 'unable to parse requirement' "$T/notforbidden.out" && ! grep -q 'cluster-reader' "$T/notforbidden.out" && ok "status: a node-list failure that is not a Forbidden reports kubectl's reason, not the permission story" || fail "status non-Forbidden failure: $(tail -1 "$T/notforbidden.out")"; fi

# no DaemonSets and no --image: a clear refusal, not a silent empty walk
if STUB_DAEMONSETS="" PATH="$STUB_PATH" bash deploy/prepull.sh status -n check-ns > "$T/none.out" 2>&1; then fail "status with nothing to check must fail"; else
    grep -q 'no pre-pull DaemonSets' "$T/none.out" && ok "status: no DaemonSets and no --image is refused with a reason" || fail "status: refusal message missing: $(tail -1 "$T/none.out")"; fi

# no holder anywhere: the DaemonSet's FailedCreate event is the explanation
printf '{"items":[]}' > "$T/pods-none.json"
cp "$T/pods.json" "$T/pods-all.json"; cp "$T/pods-none.json" "$T/pods.json"
if STUB_EVENT='pods "prepull-x-" is forbidden: violates PodSecurity "restricted:latest": runAsNonRoot != true' PATH="$STUB_PATH" bash deploy/prepull.sh status -n check-ns --image "$IMG" > "$T/nopods.out" 2>&1; then fail "status with no holders must fail"; else
    grep -q 'cannot create its pods: pods "prepull-x-" is forbidden: violates PodSecurity' "$T/nopods.out" && ok "status: with no holder anywhere the DaemonSet's FailedCreate event is printed" || fail "status: FailedCreate event missing: $(tail -2 "$T/nopods.out")"; fi
cp "$T/pods-all.json" "$T/pods.json"

# a node list the caller may not read: an error alone, a warning after apply
if STUB_NODES_FORBIDDEN=1 PATH="$STUB_PATH" bash deploy/prepull.sh status -n check-ns --image "$IMG" > "$T/forbidden.out" 2>&1; then fail "status must fail when nodes cannot be listed"; else
    grep -q 'cluster-reader' "$T/forbidden.out" && ok "status: a Forbidden node list fails with the permission named" || fail "status: Forbidden handling: $(tail -1 "$T/forbidden.out")"; fi
if STUB_NODES_FORBIDDEN=1 PATH="$STUB_PATH" bash deploy/prepull.sh apply -n check-ns --image "$IMG" > "$T/apply-forbidden.out" 2>&1; then
    grep -q 'cannot list nodes' "$T/apply-forbidden.out" && ! grep -q '0/ nodes' "$T/apply-forbidden.out" && ok "apply: a Forbidden node list warns and does not print a 0/ tally" || fail "apply on Forbidden: $(tail -2 "$T/apply-forbidden.out")"
else
    fail "apply must succeed when the report cannot list nodes"
fi
# one image's placement problem does not hide the next image's report
STUB_NODES_FORBIDDEN=1 PATH="$STUB_PATH" bash deploy/prepull.sh apply -n check-ns --image "$IMG" --image ghcr.io/other/engine:1 > "$T/apply-two.out" 2>&1 || true
[ "$(grep -c 'cannot list nodes' "$T/apply-two.out")" -eq 2 ] && ok "apply: with two images, the report continues past the first image's problem" || fail "apply two images under Forbidden: $(grep -c 'cannot list nodes' "$T/apply-two.out") warning(s)"

# no jq: apply still succeeds (the report is skipped with a warning), status refuses
mkdir -p "$T/nojq"
for tool in bash sed awk tr printf cat grep head cut mktemp rm sort uniq shasum sha256sum env dirname basename readlink date id uname wc true false; do
    p="$(command -v "$tool" 2>/dev/null || true)"; [ -n "$p" ] && ln -sf "$p" "$T/nojq/$tool"
done
if out="$(PATH="$T/bin:$T/nojq" bash deploy/prepull.sh apply -n check-ns --image "$IMG" 2>&1)"; then
    case "$out" in *"jq is not installed"*) ok "apply: without jq the apply succeeds and the report is skipped with a warning" ;; *) fail "apply without jq: $out" ;; esac
else
    fail "apply must not fail for a missing jq after the DaemonSet was applied: $out"
fi
if PATH="$T/bin:$T/nojq" bash deploy/prepull.sh status -n check-ns --image "$IMG" >/dev/null 2>&1; then fail "status without jq must fail"; else ok "status: without jq refuses with a reason"; fi

# no node matches: verdict mode exits, report mode (after apply) warns and continues
cp "$T/nodes.json" "$T/nodes-all.json"; printf '{"items":[]}' > "$T/nodes.json"
if PATH="$STUB_PATH" bash deploy/prepull.sh status -n check-ns --image "$IMG" >/dev/null 2>&1; then fail "status with no matching node must fail"; else ok "status: no matching node is an error"; fi
if PATH="$STUB_PATH" bash deploy/prepull.sh apply -n check-ns --image "$IMG" > "$T/apply.out" 2>&1; then
    grep -q 'held nowhere' "$T/apply.out" && ok "apply: a placement matching no node warns and still succeeds" || fail "apply: the no-node warning is missing"
else
    fail "apply must not fail because no node matched (log_error inside status ended the process)"
fi
cp "$T/nodes-all.json" "$T/nodes.json"

# the selector reaches kubectl as given
: > "$T/calls"
STUB_SELECTOR=example.com/accelerator=h200 PATH="$STUB_PATH" bash deploy/prepull.sh status -n check-ns --image "$IMG" --node-selector example.com/accelerator=h200 >/dev/null 2>&1 || true
grep -q 'get nodes -l example.com/accelerator=h200' "$T/calls" && ok "status: --node-selector is what kubectl is asked with" || fail "status: selector not passed: $(grep 'get nodes' "$T/calls")"
# a live nodeSelector of two keys reads back as both
: > "$T/calls"
STUB_DS_SELECTOR="$SEL2" STUB_SELECTOR="$SEL2" PATH="$STUB_PATH" bash deploy/prepull.sh status -n check-ns --image "$IMG" > "$T/sel2-live.out" 2>&1 || true
grep -q "get nodes -l ${SEL2} " "$T/calls" && ok "status: every pair of the live nodeSelector reaches the node listing" || fail "status: two-key live selector read back as: $(grep 'get nodes' "$T/calls"); $(tail -1 "$T/sel2-live.out")"
# a node matches only when it carries every pair
cat > "$T/nodes-sel2.json" <<'EOF'
{"items":[
 {"metadata":{"name":"node-both",        "labels":{"example.com/accelerator":"h200","example.com/state":"production"}}},
 {"metadata":{"name":"node-accel-only",  "labels":{"example.com/accelerator":"h200"}}},
 {"metadata":{"name":"node-state-only",  "labels":{"example.com/state":"production"}}},
 {"metadata":{"name":"node-other-state", "labels":{"example.com/accelerator":"h200","example.com/state":"broken"}}}
]}
EOF
STUB_NODES_FILE="$T/nodes-sel2.json" STUB_SELECTOR="$SEL2" PATH="$STUB_PATH" bash deploy/prepull.sh status -n check-ns --image "$IMG" --node-selector "$SEL2" > "$T/sel2-status.out" 2>&1 || true
grep -q '^  node-both ' "$T/sel2-status.out" && ! grep -Eq 'node-(accel-only|state-only|other-state)' "$T/sel2-status.out" \
    && ok "status: a node carrying only one of the pairs is not counted" || fail "status two-pair node rows: $(grep -o 'node-[a-z-]*' "$T/sel2-status.out" | sort -u | tr '\n' ' ')"

# delete: by name for --image, by both labels for --all; --dry-run makes no call
: > "$T/calls"
PATH="$STUB_PATH" bash deploy/prepull.sh delete -n check-ns --image "$IMG" >/dev/null 2>&1
grep -q "delete daemonset -n check-ns ${DS} --ignore-not-found" "$T/calls" && ok "delete --image: deletes the DaemonSet name_for derives, in the namespace" || fail "delete --image issued: $(grep delete "$T/calls")"
: > "$T/calls"
PATH="$STUB_PATH" bash deploy/prepull.sh delete -n check-ns --all >/dev/null 2>&1
grep -q "delete daemonset -n check-ns -l ${LBL} --ignore-not-found" "$T/calls" && ok "delete --all: deletes by component AND managed-by, in the namespace" || fail "delete --all issued: $(grep delete "$T/calls")"
: > "$T/calls"
out="$(PATH="$STUB_PATH" bash deploy/prepull.sh delete -n check-ns --all --dry-run 2>&1 || true)"
if [ ! -s "$T/calls" ] && [ "${out#would run: kubectl delete daemonset}" != "$out" ]; then ok "delete --dry-run: prints the delete and makes no kubectl call"; else fail "delete --dry-run: calls=$(cat "$T/calls") out=$out"; fi
if bash deploy/prepull.sh apply -n check-ns --image "$IMG" --all --dry-run >/dev/null 2>&1; then fail "apply must refuse --all"; else ok "apply: --all is refused (a delete option)"; fi

# an image reference that is not one is refused before anything is rendered or applied
: > "$T/calls"
for badimg in 'a b' 'a"b' 'a,b' ''; do
    if PATH="$STUB_PATH" bash deploy/prepull.sh apply -n check-ns --image "$badimg" >/dev/null 2>&1; then fail "apply accepted image '$badimg'"; fi
done
[ ! -s "$T/calls" ] && ok "apply: a quote, a space, a comma or an empty --image is refused with no kubectl call" || fail "apply: kubectl was called for a refused image: $(cat "$T/calls")"
out="$(bash deploy/prepull.sh apply -n check-ns --image 2>&1 || true)"
case "$out" in *'needs a value'*) ok "apply: --image as the last argument is a usage error" ;; *) fail "apply: --image with no value is not refused cleanly: $out" ;; esac

# ---------------------------------------------------------------------------
# 4. The Makefile: the comma lists, the NAMESPACE guard, and the standup's
#    pre-pull step -- the recipe fragment itself, executed against the stub,
#    since `make -n` prints the same shape whatever BENCHMARK_PREPULL is.
# ---------------------------------------------------------------------------
line="$(make -n prepull IMAGES=a:1,b:2 NAMESPACE=ns PREPULL_TOLERATIONS=t1,t2 2>/dev/null | grep 'prepull.sh apply' || true)"
case "$line" in
    *"--image a:1 --image b:2"*) ok "make prepull: IMAGES=a,b becomes --image a --image b" ;;
    *) fail "make prepull expanded to: $line" ;;
esac
case "$line" in
    *"--toleration t1 --toleration t2"*) ok "make prepull: PREPULL_TOLERATIONS=a,b becomes --toleration a --toleration b" ;;
    *) fail "make prepull tolerations: $line" ;;
esac
case "$line" in
    *"--node-selector"*) fail "make prepull: an empty PREPULL_NODE_SELECTOR must pass no --node-selector: $line" ;;
    *) ok "make prepull: no PREPULL_NODE_SELECTOR means the product-key affinity (no --node-selector)" ;;
esac
line="$(make -n prepull IMAGES=a:1 NAMESPACE=ns PREPULL_NODE_SELECTOR=k=v 2>/dev/null | grep 'prepull.sh apply' || true)"
case "$line" in *'--node-selector "k=v"'*) ok "make prepull: PREPULL_NODE_SELECTOR is passed through" ;; *) fail "make prepull selector: $line" ;; esac
line="$(make -n prepull-delete NAMESPACE=ns 2>/dev/null | grep 'prepull.sh delete' || true)"
case "$line" in *"--all"*) ok "make prepull-delete with no IMAGES deletes every holder" ;; *) fail "prepull-delete: $line" ;; esac
composed="$(printf '%s' "a:1,b:2" | tr ',' '\n' | sed 's/^/--image /' | tr '\n' ' ')"
[ "$composed" = "--image a:1 --image b:2" ] && ok "standup: the tr/sed composition yields --image a --image b" || fail "standup composition: '$composed'"
# NAMESPACE has a Makefile default; the targets must not take it silently.
# Run, not `make -n`: the guard is a recipe line, and -n prints it unfired.
: > "$T/calls"
if PATH="$STUB_PATH" make prepull-status > "$T/make-nons.out" 2>&1; then fail "make prepull-status without NAMESPACE ran against the Makefile default"; else
    [ ! -s "$T/calls" ] && grep -q 'set NAMESPACE' "$T/make-nons.out" && ok "make prepull-status without NAMESPACE is refused before any kubectl call" || fail "make prepull-status without NAMESPACE: $(tail -1 "$T/make-nons.out"); calls: $(cat "$T/calls")"; fi
: > "$T/calls"
PATH="$STUB_PATH" make prepull-status NAMESPACE=check-ns > "$T/make-ns.out" 2>&1 || true
grep -q '^  node-running ' "$T/make-ns.out" && ok "make prepull-status NAMESPACE=<ns> runs the status against that namespace" || fail "make prepull-status NAMESPACE=check-ns: $(tail -2 "$T/make-ns.out")"
NAMESPACE=check-ns PATH="$STUB_PATH" make prepull-status > "$T/make-env.out" 2>&1 || true
grep -q '^  node-running ' "$T/make-env.out" && ok "make prepull-status takes an exported NAMESPACE too (the guides' export convention, as model-cache does)" || fail "make prepull-status with an exported NAMESPACE: $(tail -2 "$T/make-env.out")"

# the standup's pre-pull block, executed: the namespace is created before
# the apply, BENCHMARK_PREPULL=false skips, BENCHMARK_PREPULL_IMAGES overrides
# tr -d: a CRLF checkout (Windows) turns each recipe continuation into a
# backslash-CR, which bash does not read as a continuation.
block="$(tr -d '\r' < Makefile | sed -n '/The engine image on every accelerator node BEFORE/,/Lives in a script, not inline here/p' | sed '1d;$d' | grep -v '^	@#' | sed 's/^	@//; s/\$\$/$/g; s/\$(BENCHMARK_PREPULL)/${BENCHMARK_PREPULL}/g; s/\$(BENCHMARK_PREPULL_IMAGES)/${BENCHMARK_PREPULL_IMAGES}/g; s/\$(BENCHMARK_REPO_DIR)/${BENCHMARK_REPO_DIR}/g; s/\$(BENCHMARK_SPEC)/${BENCHMARK_SPEC}/g; s/\$(BENCHMARK_NAMESPACE)/${BENCHMARK_NAMESPACE}/g; s/\$(BENCHMARK_KUBECTL_TIMEOUT)/${BENCHMARK_KUBECTL_TIMEOUT}/g; s/\$(PREPULL_ARGS)//g')"
[ -n "$block" ] || fail "could not extract the benchmark-standup pre-pull block from the Makefile"
# A make variable the substitution list above does not know would reach
# bash as a command substitution -- `$(CURDIR)` runs a command named CURDIR,
# prints "command not found" to stderr and, as an argument, trips nothing.
# Refuse to run such a block rather than replay a mistranslation.
if printf '%s' "$block" | grep -Eq '[$][(][A-Z_]+[)]'; then
    fail "the extracted standup block still holds a make reference the check does not translate: $(printf '%s' "$block" | grep -Eo '[$][(][A-Z_]+[)]' | sort -u | tr '
' ' ')"
else
    ok "standup: the extracted block holds no untranslated make reference (a shell substitution is lowercase)"
fi
# The standup copies the scenario into the clone before this block runs; the
# check's clone holds one with no engine override, so the default is read.
mkdir -p "$T/clone/config/scenarios/check"
cp "$T/sc-none.yaml" "$T/clone/config/scenarios/check/spec.yaml"
run_block() { env BENCHMARK_NAMESPACE=check-ns BENCHMARK_REPO_DIR="$T/clone" BENCHMARK_SPEC=check/spec BENCHMARK_KUBECTL_TIMEOUT=30s PATH="$STUB_PATH" "$@" bash -c "$block"; }
: > "$T/calls"; : > "$T/timeouts"
run_block BENCHMARK_PREPULL=true BENCHMARK_PREPULL_IMAGES= > "$T/standup.out" 2>&1 || fail "standup block failed: $(cat "$T/standup.out")"
[ "$(sort -u "$T/timeouts")" = "--request-timeout=30s" ] && ok "standup: the pre-pull step's kubectl requests carry the request timeout" || fail "standup: request timeouts seen: '$(sort -u "$T/timeouts" | tr '\n' ' ')'"
if grep -q "^create namespace check-ns" "$T/calls" && grep -q "^apply -n check-ns" "$T/calls" \
   && [ "$(grep -n '^create namespace' "$T/calls" | head -1 | cut -d: -f1)" -lt "$(grep -n '^apply -n' "$T/calls" | head -1 | cut -d: -f1)" ]; then
    ok "standup: the namespace is created before the pre-pull apply"
else
    fail "standup: namespace create missing or after the apply: $(tr '\n' ';' < "$T/calls")"
fi
grep -q "Holding the engine image.*${IMG}" "$T/standup.out" && ok "standup: the image comes from the clone's defaults.yaml" || fail "standup: image not read from the clone: $(head -3 "$T/standup.out")"
: > "$T/calls"
run_block BENCHMARK_PREPULL=false BENCHMARK_PREPULL_IMAGES= > "$T/standup-skip.out" 2>&1 || true
[ ! -s "$T/calls" ] && ok "standup: BENCHMARK_PREPULL=false makes no kubectl call" || fail "standup: BENCHMARK_PREPULL=false still called kubectl: $(tr '\n' ';' < "$T/calls")"
: > "$T/calls"
run_block BENCHMARK_PREPULL=true BENCHMARK_PREPULL_IMAGES=override/engine:9 > "$T/standup-override.out" 2>&1 || true
grep -q 'Holding the engine image.*override/engine:9' "$T/standup-override.out" && ok "standup: BENCHMARK_PREPULL_IMAGES overrides the clone's pin" || fail "standup override: $(head -2 "$T/standup-override.out")"
: > "$T/calls"
mkdir -p "$T/clone/config/scenarios/check-pin"
cp "$T/sc-shared.yaml" "$T/clone/config/scenarios/check-pin/spec.yaml"
run_block BENCHMARK_PREPULL=true BENCHMARK_PREPULL_IMAGES= BENCHMARK_SPEC=check-pin/spec > "$T/standup-pin.out" 2>&1 || true
grep -qF 'Holding the engine image on the accelerator nodes: docker.io/vllm/vllm-openai:v0.31.0 (' "$T/standup-pin.out" && ok "standup: a scenario's engine pin is the image held on the nodes" || fail "standup with a scenario pin: $(head -2 "$T/standup-pin.out")"
: > "$T/calls"
run_block BENCHMARK_PREPULL=true BENCHMARK_PREPULL_IMAGES= BENCHMARK_REPO_DIR="$T/nonexistent" > "$T/standup-noclone.out" 2>&1 || true
grep -q 'WARNING: could not read the engine image' "$T/standup-noclone.out" && [ ! -s "$T/calls" ] && ok "standup: no clone means a warning and no apply" || fail "standup without a clone: $(head -2 "$T/standup-noclone.out")"

# ---------------------------------------------------------------------------
# 5. The scenarios: engine containers pull IfNotPresent, the harness's init
#    container is left as it was.
# ---------------------------------------------------------------------------
"$PY" - hack/benchmark/scenarios/guides/*.yaml <<'PYEOF' || FAILED=1
import sys, yaml
bad = []
for path in sys.argv[1:]:
    d = yaml.safe_load(open(path, encoding="utf-8"))
    engines = inits = 0
    def walk(o):
        global engines, inits
        if isinstance(o, dict):
            ecc = o.get("extraContainerConfig")
            if isinstance(ecc, dict) and "imagePullPolicy" in ecc:
                engines += 1
                if ecc["imagePullPolicy"] != "IfNotPresent":
                    bad.append("%s: engine container pulls %s" % (path, ecc["imagePullPolicy"]))
            for ic in o.get("initContainers") or []:
                if isinstance(ic, dict) and ic.get("name") == "preprocess":
                    inits += 1
                    if ic.get("imagePullPolicy") != "Always":
                        bad.append("%s: the preprocess init container was changed to %s" % (path, ic.get("imagePullPolicy")))
            for v in o.values():
                walk(v)
        elif isinstance(o, list):
            for v in o:
                walk(v)
    walk(d)
    if engines == 0:
        bad.append("%s: no engine container pull policy found (extraContainerConfig.imagePullPolicy)" % path)
if bad:
    for b in bad:
        print("  FAIL " + b)
    sys.exit(1)
print("  ok   every scenario's engine container pulls IfNotPresent; the preprocess init container still pulls Always")
PYEOF

if [ "$FAILED" -ne 0 ]; then
    echo "prepull checks: FAIL"
    exit 1
fi
echo "prepull checks OK"
