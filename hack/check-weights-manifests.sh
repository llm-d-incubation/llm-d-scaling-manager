#!/usr/bin/env bash
# Executes deploy/weights.sh and hack/benchmark/model_hostpath.sh against
# fixtures and asserts what they do.
#
# Every defect this covers parses fine: a volume without a claimRef (the
# first claim asking for the class takes it), a claim larger than its volume
# (never binds), a downloader that mounts a hostPath directly (Pod Security
# baseline refuses it; via the claim it is admitted), a readiness probe on
# the wrong path (Ready before the download ends, which is what the harness
# and `status` both read), a model id that reaches a Python string or a path
# unchecked, a `status` that reads a still-downloading node as present, and
# a scenario edit that lands the hostPath block somewhere the harness does
# not read or misses the second layout the scenarios use.
#
# `status`, `delete` and the scenario edit run offline against a stub
# kubectl on PATH, the pattern hack/check-prepull-manifests.sh uses.
set -euo pipefail

command -v jq >/dev/null 2>&1 || { printf 'FATAL: jq is required to run these checks.\n' >&2; exit 1; }
command -v yq >/dev/null 2>&1 || { printf 'FATAL: yq is required to run these checks (the scenario edit uses it).\n' >&2; exit 1; }
cd "$(dirname "$0")/.."

PY=${PYTHON:-python3}
T="$(mktemp -d)"
trap 'rm -rf "$T"' EXIT
FAILED=0
ok()   { printf '  ok   %s\n' "$1"; }
fail() { printf '  FAIL %s\n' "$1"; FAILED=1; }

# ---------------------------------------------------------------------------
# 1. The rendered volume, claim and downloader.
# ---------------------------------------------------------------------------
IMG=docker.io/vllm/vllm-openai:v0.26.0
bash deploy/weights.sh apply -n check-ns --model Qwen/Qwen3-32B --path /mnt/local/models --image "$IMG" \
    --hf-token-secret hf-token/token --capacity 2Ti --node-selector example.com/accelerator=h200 --toleration example.com/dedicated \
    --dry-run > "$T/render.yaml"
bash deploy/weights.sh apply -n check-ns --model Qwen/Qwen3-32B --path /mnt/local/models --image "$IMG" --dry-run > "$T/render-default.yaml"
bash deploy/weights.sh apply -n check-ns --model gpt2 --path /mnt/local/models --image "$IMG" --hf-token-secret myonlysecret --dry-run > "$T/render-secret.yaml"
# the same model in another namespace: a different volume, the same claim name
bash deploy/weights.sh apply -n other-ns --model Qwen/Qwen3-32B --path /mnt/local/models --image "$IMG" --dry-run > "$T/render-other.yaml"

"$PY" - "$T/render.yaml" "$T/render-default.yaml" "$T/render-other.yaml" deploy/lib/accelerator_labels.py "$T/render-secret.yaml" <<'PYEOF' || FAILED=1
import re, sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1], encoding="utf-8")) if d]
default = [d for d in yaml.safe_load_all(open(sys.argv[2], encoding="utf-8")) if d]
other = [d for d in yaml.safe_load_all(open(sys.argv[3], encoding="utf-8")) if d]
secret = [d for d in yaml.safe_load_all(open(sys.argv[5], encoding="utf-8")) if d]
ns = {}
exec(open(sys.argv[4], encoding="utf-8").read(), ns)
product_keys = ns["PRODUCT_KEYS"]
bad = []
def check(cond, msg):
    if not cond:
        bad.append(msg)

check([d["kind"] for d in docs] == ["ServiceAccount", "PersistentVolume", "PersistentVolumeClaim", "DaemonSet"], "kinds: %s" % [d["kind"] for d in docs])
sa, pv, pvc, ds = docs
check(sa["metadata"]["name"] == "weights-downloader" and sa["metadata"]["namespace"] == "check-ns" and sa.get("automountServiceAccountToken") is False,
      "a ServiceAccount of its own, with no token: %s" % sa)
check(ds["spec"]["template"]["spec"].get("serviceAccountName") == "weights-downloader", "the downloader runs as its own ServiceAccount, so a grant for it does not land on the default one")
label = re.compile(r"^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$")
check(bool(label.match(pvc["metadata"]["name"])) and bool(label.match(ds["metadata"]["name"])), "claim/DaemonSet names are DNS-1123 labels")
check(pvc["metadata"]["name"] == ds["metadata"]["name"], "claim and DaemonSet share a name")
check(pv["spec"].get("claimRef") == {"namespace": "check-ns", "name": pvc["metadata"]["name"]}, "the volume is bound to this claim and no other: %s" % pv["spec"].get("claimRef"))
check(pvc["spec"]["volumeName"] == pv["metadata"]["name"], "the claim names the volume")
check(pv["spec"]["storageClassName"] == pvc["spec"]["storageClassName"] == "node-local-weights", "one synthetic storage class on both sides")
check(pv["spec"]["capacity"]["storage"] == pvc["spec"]["resources"]["requests"]["storage"] == "2Ti", "--capacity reaches both, equal (a larger request never binds)")
check(pv["spec"]["persistentVolumeReclaimPolicy"] == "Retain", "Retain: the files are the point")
check(pv["spec"]["hostPath"] == {"path": "/mnt/local/models", "type": "DirectoryOrCreate"}, "hostPath: %s" % pv["spec"]["hostPath"])
check(pv["spec"]["accessModes"] == ["ReadWriteMany"] == pvc["spec"]["accessModes"], "RWX on both: every node mounts the same claim")
for d in docs:
    check(d["metadata"]["labels"].get("app.kubernetes.io/managed-by") == "scaling-manager-weights", "managed-by on %s" % d["kind"])
for d in (pv, pvc, ds):
    check(d["metadata"].get("annotations", {}).get("scaling-manager.llmd.ai/weights-model") == "Qwen/Qwen3-32B", "annotation names the model on %s" % d["kind"])
check(ds["metadata"]["annotations"]["scaling-manager.llmd.ai/weights-node-selector"] == "example.com/accelerator=h200", "the DaemonSet records the selector")
tpl = ds["spec"]["template"]; ps = tpl["spec"]
check(tpl["metadata"]["labels"].get("app.kubernetes.io/managed-by") == "scaling-manager-weights", "pods carry managed-by")
check(all(tpl["metadata"]["labels"].get(k) == v for k, v in ds["spec"]["selector"]["matchLabels"].items()), "selector matches the template labels")
check(ps["nodeSelector"] == {"example.com/accelerator": "h200"} and "affinity" not in ps, "an explicit selector is a nodeSelector, no affinity")
check({t["key"] for t in ps["tolerations"]} == {"nvidia.com/gpu", "example.com/dedicated"}, "tolerations: %s" % ps["tolerations"])
check(ps.get("automountServiceAccountToken") is False, "no ServiceAccount token")
check((ps.get("securityContext") or {}).get("seccompProfile", {}).get("type") == "RuntimeDefault", "seccomp RuntimeDefault")
vols = ps["volumes"]
check(len(vols) == 1 and vols[0].get("persistentVolumeClaim", {}).get("claimName") == pvc["metadata"]["name"], "the downloader mounts the CLAIM, not a hostPath: %s" % vols)
check(all("hostPath" not in v for v in vols), "no hostPath volume in the pod (Pod Security baseline)")
c = ps["containers"][0]
check(c["image"] == "docker.io/vllm/vllm-openai:v0.26.0" and c["imagePullPolicy"] == "IfNotPresent", "downloads with the given image, IfNotPresent")
env = {e["name"]: e for e in c["env"]}
check(env["MODEL_ID"]["value"] == "Qwen/Qwen3-32B", "MODEL_ID")
check(env["TARGET_DIR"]["value"] == "/weights/models/Qwen/Qwen3-32B", "the model lands under <mount>/models/<id>, the harness's layout: %s" % env["TARGET_DIR"]["value"])
check(env["NVIDIA_VISIBLE_DEVICES"]["value"] == "void", "no GPU injected into the downloader")
check(env["HF_TOKEN"]["valueFrom"]["secretKeyRef"] == {"name": "hf-token", "key": "token"}, "the token comes from the named Secret and key: %s" % env["HF_TOKEN"])
mount = c["volumeMounts"][0]
check(mount["name"] == vols[0]["name"] and mount["mountPath"] == "/weights", "the claim is mounted at /weights")
probe = c["readinessProbe"]["exec"]["command"]
check(probe == ["test", "-f", "/weights/models/Qwen/Qwen3-32B/.download-complete"], "Ready is the completion marker under the mount: %s" % probe)
check(env["MARKER"]["value"] == ".download-complete", "the marker the script writes is the one the probe tests")
script = c["args"][0]
check("snapshot_download" in script and "$(" not in script and "$$" not in script, "the download script uses huggingface_hub and carries no $( or $$ (Kubernetes rewrites both)")
check(c["command"] == ["/bin/sh", "-c"], "sh -c with the script as args")
res = c.get("resources", {})
for section in ("requests", "limits"):
    for k in res.get(section, {}):
        check("gpu" not in k and "/" not in k, "the downloader requests an accelerator: %s" % k)
check(res.get("limits", {}).get("memory"), "a memory limit")
csc = c.get("securityContext", {})
check(csc.get("allowPrivilegeEscalation") is False and "ALL" in csc.get("capabilities", {}).get("drop", []), "drops all capabilities")

check(len(default) == 4, "defaults render four documents")
if len(default) == 4:
    dps = default[3]["spec"]["template"]["spec"]
    check("nodeSelector" not in dps, "no --node-selector: no nodeSelector")
    terms = dps.get("affinity", {}).get("nodeAffinity", {}).get("requiredDuringSchedulingIgnoredDuringExecution", {}).get("nodeSelectorTerms", [])
    keys = [t["matchExpressions"][0]["key"] for t in terms if len(t.get("matchExpressions", [])) == 2]
    check(keys == product_keys, "the default affinity is the product-key list: %s" % keys)
    check(default[2]["spec"]["resources"]["requests"]["storage"] == "1Ti", "default capacity 1Ti")
    check("HF_TOKEN" not in {e["name"] for e in dps["containers"][0]["env"]}, "no --hf-token-secret: no HF_TOKEN env")
    check(dps["tolerations"] == [{"key": "nvidia.com/gpu", "operator": "Exists"}], "default toleration")
if len(secret) == 4:
    senv = {e["name"]: e for e in secret[3]["spec"]["template"]["spec"]["containers"][0]["env"]}
    check(senv["HF_TOKEN"]["valueFrom"]["secretKeyRef"] == {"name": "myonlysecret", "key": "HF_TOKEN"}, "a Secret named without /KEY uses the key HF_TOKEN: %s" % senv["HF_TOKEN"])
    check(senv["TARGET_DIR"]["value"] == "/weights/models/gpt2", "a model id with no org lands under models/<id>: %s" % senv["TARGET_DIR"]["value"])
if len(other) == 4:
    check(other[1]["metadata"]["name"] != default[1]["metadata"]["name"], "the same model in another namespace gets its own (cluster-scoped) volume")
    check(other[2]["metadata"]["name"] == default[2]["metadata"]["name"], "and the same claim name (namespace-scoped)")
    check(other[1]["spec"]["claimRef"]["namespace"] == "other-ns", "bound into its own namespace")

if bad:
    for b in bad:
        print("  FAIL " + b)
    sys.exit(1)
print("  ok   volume, claim and downloader rendered: bound pair, claim-mounted (no hostPath in the pod), marker probe, layout, placement, no GPU, no token")
PYEOF

refused=0
for badmodel in 'a/b/c' '/x' 'x/' 'a b' 'a"b' '' '..' 'a/..' '../x' 'Qwen/..' '.hidden' 'Qwen--x' 'a-/b' 'a/b.'; do
    if bash deploy/weights.sh apply -n check-ns --model "$badmodel" --path /mnt/local/models --image "$IMG" --dry-run >/dev/null 2>&1; then fail "a --model of '$badmodel' must be refused"; refused=1; fi
done
[ "$refused" -eq 0 ] && ok "a model id that is not [org/]name of Hugging Face characters, or carries .. / -- / a segment edge of . or -, is refused"
refused=0
for badpath in relative / 'a b' '/p"' '' /mnt /etc /etc/models /var/lib/kubelet /var/lib/containerd/x /tmp/models /home/me/models /proc/1 /mnt/local/models/ /mnt/local/../x /var/home/core/w /var/roothome/w /var/usrlocal/w /var/opt/cni/w /sysroot/ostree/x /ostree/x /var/mnt /var/srv /opt/bin/x /var/spool/cron /var/opt/bin/x; do
    if bash deploy/weights.sh apply -n check-ns --model m --path "$badpath" --image "$IMG" --dry-run >/dev/null 2>&1; then fail "a --path of '$badpath' must be refused"; refused=1; fi
done
[ "$refused" -eq 0 ] && ok "a path that is not absolute, has one component, ends in /, holds .., or sits under a system prefix (/etc, /var/lib, /tmp, /home, ...) is refused"
for goodpath in /mnt/local/models /data/models /opt/models /srv/weights /var/mnt/weights /var/srv/weights /var/data/weights; do
    bash deploy/weights.sh apply -n check-ns --model m --path "$goodpath" --image "$IMG" --dry-run >/dev/null 2>&1 || fail "a --path of '$goodpath' must be accepted"
done
ok "a data directory of two or more components outside the system prefixes is accepted, RHCOS's /var/mnt and /var/srv included"
# the refusal loops prove nothing if the good form is refused too
bash deploy/weights.sh apply -n check-ns --model m --path /mnt/local/models --image "$IMG" --dry-run >/dev/null 2>&1 && ok "the form the refusal tests vary is itself accepted" || fail "the refusal tests' base invocation is refused, so they prove nothing"
if bash deploy/weights.sh apply -n check-ns --model m --path /mnt/local/models --image "$IMG" --hf-token-secret 'a b' --dry-run >/dev/null 2>&1; then fail "a Secret reference with a space must be refused"; else ok "a Secret reference that is not NAME[/KEY] is refused"; fi
if bash deploy/weights.sh apply -n check-ns --model m --path /mnt/local/models --image "$IMG" --capacity '1Ti; rm' --dry-run >/dev/null 2>&1; then fail "a capacity that is not a quantity must be refused"; else ok "a capacity that is not a quantity is refused"; fi
if bash deploy/weights.sh apply -n check-ns --model m --path /mnt/local/models --dry-run >/dev/null 2>&1; then fail "apply without --image must be refused"; else ok "apply without --image is refused"; fi
if bash deploy/weights.sh apply -n check-ns --model m --path /mnt/local/models --image "$IMG" --all --dry-run >/dev/null 2>&1; then fail "apply must refuse --all"; else ok "apply: --all is refused"; fi
# The downloader's memory limit. At 2Gi it was OOMKilled on every node fetching
# Qwen/Qwen3-8B and Meta-Llama-3.1-8B, and restarted forever: the default must
# cover an 8B model, and --memory must reach the container.
mem_limit() { yq -r 'select(.kind == "DaemonSet") | .spec.template.spec.containers[0].resources.limits.memory' "$1"; }
mem_default="$(mem_limit "$T/render-default.yaml")"
case "$mem_default" in
    *Gi) [ "${mem_default%Gi}" -ge 16 ] 2>/dev/null && ok "the downloader's default memory limit is $mem_default, enough for an 8B model" || fail "the downloader's default memory limit is $mem_default; 2Gi was OOMKilled on an 8B model" ;;
    *) fail "the downloader's default memory limit is '$mem_default', not a Gi quantity of at least 16" ;;
esac
bash deploy/weights.sh apply -n check-ns --model Qwen/Qwen3-32B --path /mnt/local/models --image "$IMG" --memory 48Gi --dry-run > "$T/render-mem.yaml"
[ "$(mem_limit "$T/render-mem.yaml")" = 48Gi ] && ok "--memory sets the downloader's memory limit" || fail "--memory 48Gi rendered a limit of '$(mem_limit "$T/render-mem.yaml")'"
if bash deploy/weights.sh apply -n check-ns --model m --path /mnt/local/models --image "$IMG" --memory 'lots' --dry-run >/dev/null 2>&1; then fail "apply must refuse --memory that is not a quantity"; else ok "apply: --memory that is not a quantity is refused"; fi
# Several KEY=VALUE pairs, comma separated: a node must carry every one. Where
# no single label picks out the usable GPU nodes, the product label AND a
# second one does. Every pair is a nodeSelector entry, in the order given, and
# the annotation reads back as the flag was given.
SEL2=example.com/accelerator=h200,example.com/state=production
if bash deploy/weights.sh apply -n check-ns --model Qwen/Qwen3-32B --path /mnt/local/models --image "$IMG" --node-selector "$SEL2" --dry-run > "$T/render-sel2.yaml" 2>&1; then
    [ "$(yq -o json -I0 'select(.kind == "DaemonSet") | .spec.template.spec.nodeSelector' "$T/render-sel2.yaml")" = '{"example.com/accelerator":"h200","example.com/state":"production"}' ] \
        && ok "two pairs: the nodeSelector carries both keys" || fail "two pairs rendered: $(yq -o json -I0 'select(.kind == "DaemonSet") | .spec.template.spec.nodeSelector' "$T/render-sel2.yaml")"
    [ "$(yq -r 'select(.kind == "DaemonSet") | .metadata.annotations["scaling-manager.llmd.ai/weights-node-selector"]' "$T/render-sel2.yaml")" = "$SEL2" ] \
        && [ "$(yq -r 'select(.kind == "DaemonSet") | .spec.template.spec | has("affinity")' "$T/render-sel2.yaml")" = false ] \
        && ok "two pairs: the annotation records the selector as given, and no affinity is added" || fail "two pairs: annotation or affinity wrong"
else
    fail "a two-pair --node-selector was refused: $(tail -1 "$T/render-sel2.yaml")"
fi
# one pair renders the block it always did -- the quoted key, the quoted value,
# nothing more (the key is quoted since a key such as on is a YAML 1.1 boolean)
"$PY" -c 'import re, sys; sys.exit(0 if re.search(r"\n      nodeSelector:\n        \"example\.com/accelerator\": \"h200\"\n(?!        )", open(sys.argv[1]).read()) else 1)' "$T/render.yaml" \
    && ok "one pair: the nodeSelector block is the one it always was, the key now quoted as the value is" || fail "one pair: the nodeSelector block changed"
selbad=0
for badsel in 'a=b,' ',a=b' 'a=b,,c=d' 'a=b,a=c' 'a=b c' 'a=b, c=d' 'a=b,c' 'a!=b' 'a in (b)' $'a=b\nc=d' $'a=b\n'; do
    if bash deploy/weights.sh apply -n check-ns --model m --path /mnt/local/models --image "$IMG" --node-selector "$badsel" --dry-run >/dev/null 2>&1; then
        fail "a --node-selector of '$badsel' must be refused"; selbad=1
    fi
done
[ "$selbad" -eq 0 ] && ok "a selector with an empty pair, a trailing or leading comma, a key twice, a space, a pair with no =, or a set-based term is refused"

# ---------------------------------------------------------------------------
# 2. status and delete, offline.
# ---------------------------------------------------------------------------
LBL='app.kubernetes.io/component=node-local-weights,app.kubernetes.io/managed-by=scaling-manager-weights'
NAME="$(yq -r 'select(.kind == "PersistentVolumeClaim") | .metadata.name' "$T/render-default.yaml")"
PVNAME="$(yq -r 'select(.kind == "PersistentVolume") | .metadata.name' "$T/render-default.yaml")"
[ -n "$NAME" ] && [ -n "$PVNAME" ] || fail "no claim/volume name in the rendered manifests"
cat > "$T/nodes.json" <<EOF
{"items":[
 {"metadata":{"name":"node-ready",   "labels":{"nvidia.com/gpu.product":"H200"}}},
 {"metadata":{"name":"node-loading", "labels":{"gpu.nvidia.com/model":"H200"}}},
 {"metadata":{"name":"node-failing", "labels":{"nvidia.com/gpu.product":"H200"}}},
 {"metadata":{"name":"node-nopod",   "labels":{"nvidia.com/gpu.product":"H200"}}},
 {"metadata":{"name":"node-cpu",     "labels":{"gpu.nvidia.com/model":""}}},
 {"metadata":{"name":"node-amd",     "labels":{"amd.com/gpu.product-name":"MI300X"}}},
 {"metadata":{"name":"node-evicted", "labels":{"nvidia.com/gpu.product":"H200"}}},
 {"metadata":{"name":"node-unsched", "labels":{"nvidia.com/gpu.product":"H200"}}},
 {"metadata":{"name":"node-oom",     "labels":{"nvidia.com/gpu.product":"H200"}}},
 {"metadata":{"name":"node-cordoned","labels":{"nvidia.com/gpu.product":"H200"}},"spec":{"unschedulable":true,"taints":[{"key":"node.kubernetes.io/unschedulable","effect":"NoSchedule"},{"key":"node.kubernetes.io/disk-pressure","effect":"NoSchedule"}]}},
 {"metadata":{"name":"node-foreign", "labels":{"nvidia.com/gpu.product":"H200"}},"spec":{"taints":[{"key":"example.com/team-x","value":"only","effect":"NoSchedule"}]}}
]}
EOF
# the DaemonSet as apply wrote it: it tolerates nvidia.com/gpu, nothing else
printf '%s' '{"spec":{"template":{"spec":{"tolerations":[{"key":"nvidia.com/gpu","operator":"Exists"}]}}}}' > "$T/ds.json"
cat > "$T/pods.json" <<EOF
{"items":[
 {"metadata":{"name":"dl-ready","labels":{"scaling-manager.llmd.ai/weights":"${NAME}"}},"spec":{"nodeName":"node-ready"},"status":{"phase":"Running","containerStatuses":[{"ready":true,"state":{"running":{}}}]}},
 {"metadata":{"name":"dl-loading","labels":{"scaling-manager.llmd.ai/weights":"${NAME}"}},"spec":{"nodeName":"node-loading"},"status":{"phase":"Running","containerStatuses":[{"ready":false,"state":{"running":{}}}]}},
 {"metadata":{"name":"dl-failing","labels":{"scaling-manager.llmd.ai/weights":"${NAME}"}},"spec":{"nodeName":"node-failing"},"status":{"phase":"Running","containerStatuses":[{"ready":false,"state":{"waiting":{"reason":"CrashLoopBackOff"}}}]}},
 {"metadata":{"labels":{"scaling-manager.llmd.ai/weights":"other"}},"spec":{"nodeName":"node-nopod"},"status":{"phase":"Running","containerStatuses":[{"ready":true,"state":{"running":{}}}]}},
 {"metadata":{"labels":{"scaling-manager.llmd.ai/weights":"${NAME}"}},"spec":{"nodeName":"node-evicted"},"status":{"phase":"Failed","reason":"Evicted","message":"The node had condition: [DiskPressure]."}},
 {"metadata":{"name":"dl-unsched","labels":{"scaling-manager.llmd.ai/weights":"${NAME}"}},"spec":{"affinity":{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[{"matchFields":[{"key":"metadata.name","operator":"In","values":["node-unsched"]}]}]}}}},"status":{"phase":"Pending"}},
 {"metadata":{"name":"dl-oom","labels":{"scaling-manager.llmd.ai/weights":"${NAME}"}},"spec":{"nodeName":"node-oom"},"status":{"phase":"Running","containerStatuses":[{"ready":false,"state":{"waiting":{"reason":"CrashLoopBackOff"}},"lastState":{"terminated":{"exitCode":137,"reason":"OOMKilled"}}}]}}
]}
EOF
mkdir -p "$T/bin"
cat > "$T/bin/kubectl" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "$T/calls"
ARGV=" \$* "
want() { case "\$ARGV" in *" \$1 \$2 "*) ;; *) echo "stub kubectl: \$1 \$2 missing from:\$ARGV" >&2; exit 2 ;; esac; }
refuse() { case "\$ARGV" in *" \$1 "*) echo "stub kubectl: unexpected \$1 in:\$ARGV" >&2; exit 2 ;; esac; }
case "\$1 \$2" in
  "get nodes")   if [ -n "\${STUB_SELECTOR:-}" ]; then want -l "\$STUB_SELECTOR"; else refuse -l; fi
                 [ -n "\${STUB_NODES_ERROR:-}" ] && { echo "\$STUB_NODES_ERROR" >&2; exit 1; }
                 if [ -n "\${STUB_NODES_FILE:-}" ]; then jq --arg s "\${STUB_SELECTOR:-}" '.items |= map(select((.metadata.labels // {}) as \$l | all(\$s | split(",")[]; index("=") as \$i | \$l[.[:\$i]] == .[\$i+1:])))' "\$STUB_NODES_FILE"; else cat "$T/nodes.json"; fi ;;
  "get pods")    want -n check-ns; want -l "$LBL"; cat "$T/pods.json" ;;
  "get daemonset")
      want -n check-ns
      case "\$ARGV" in
        *" -l "*) want -l "$LBL"; printf '%s\n' "\${STUB_DAEMONSETS-Qwen/Qwen3-32B}" ;;
        *" -o json "*) case "\$ARGV" in *" ${NAME} "*) ;; *) echo "stub kubectl: get daemonset for a name that is not the DaemonSet:\$ARGV" >&2; exit 2 ;; esac
           cat "$T/ds.json" ;;
        *) want -o "jsonpath={.spec.template.spec.nodeSelector}"
           case "\$ARGV" in *" ${NAME} "*) ;; *) echo "stub kubectl: get daemonset for a name that is not the DaemonSet:\$ARGV" >&2; exit 2 ;; esac
           if [ -n "\${STUB_DS_SELECTOR:-}" ]; then printf '%s' "\$STUB_DS_SELECTOR" | jq -Rc 'split(",") | map(index("=") as \$i | {key: .[:\$i], value: .[\$i+1:]}) | from_entries'; else printf '{}'; fi ;;
      esac ;;
  "get pvc")
      want -n check-ns
      case "\$ARGV" in
        *"jsonpath={.status.phase}"*) case "\$ARGV" in *" ${NAME} "*) ;; *) echo "stub kubectl: the claim read names no claim:\$ARGV" >&2; exit 2 ;; esac; printf '%s' "\${STUB_PVC_PHASE-Bound}" ;;
        *"jsonpath={range"*)
            # real kubectl: a get by NAME answers one object, whose .items is
            # empty for a range; only a get by -l answers a List
            case "\$ARGV" in *" -l "*) printf '%s\n' "\${STUB_PVC_VOLUMES-${PVNAME}}" ;; *) : ;; esac ;;
        *"jsonpath={.spec.volumeName}"*)
            case "\$ARGV" in *" -l "*) echo "stub kubectl: a single-object jsonpath on a list get:\$ARGV" >&2; exit 2 ;; esac
            case "\$ARGV" in *" ${NAME} "*) printf '%s\n' "\${STUB_PVC_VOLUMES-${PVNAME}}" ;; *) : ;; esac ;;
        *"jsonpath={.spec.resources.requests.storage}"*)
            # the size a re-apply keeps: absent is --ignore-not-found's empty
            # answer, never a failure; a failure is an error the stub names
            case "\$ARGV" in *" ${NAME} "*) ;; *) echo "stub kubectl: the size read names no claim:\$ARGV" >&2; exit 2 ;; esac
            case "\$ARGV" in *" --ignore-not-found "*) ;; *) echo "stub kubectl: the size read lacks --ignore-not-found:\$ARGV" >&2; exit 2 ;; esac
            [ -z "\${STUB_PVC_READ_ERROR:-}" ] || { echo "\$STUB_PVC_READ_ERROR" >&2; exit 1; }
            [ -z "\${STUB_PVC_PHASE-Bound}" ] || printf '%s' "\${STUB_PVC_SIZE-1Ti}" ;;
        *"jsonpath={.metadata.name}/{.spec.storageClassName}"*)
            # whose claim it is: ours is on node-local-weights
            case "\$ARGV" in *" ${NAME} "*) ;; *) echo "stub kubectl: the class read names no claim:\$ARGV" >&2; exit 2 ;; esac
            case "\$ARGV" in *" --ignore-not-found "*) ;; *) echo "stub kubectl: the class read lacks --ignore-not-found:\$ARGV" >&2; exit 2 ;; esac
            [ -z "\${STUB_PVC_READ_ERROR:-}" ] || { echo "\$STUB_PVC_READ_ERROR" >&2; exit 1; }
            [ -z "\${STUB_PVC_PHASE-Bound}" ] || printf '%s/%s' "${NAME}" "\${STUB_PVC_CLASS-node-local-weights}" ;;
        *"model-pvc"*) printf '%s' "\${STUB_MODEL_PVC_CLASS:-}" ;;
        *) echo "stub kubectl: unexpected pvc read:\$ARGV" >&2; exit 2 ;;
      esac ;;
  "get events")  want -n check-ns; printf '%s' "\${STUB_EVENT:-}" ;;
  "logs -n")     want -n check-ns; printf '%s\n' "\${STUB_LOG:-}" ;;
  "auth can-i")  case "\$ARGV" in *" create persistentvolumes -A -q "*) [ -z "\${STUB_NO_PV:-}" ] || exit 1; exit 0 ;; *) echo "stub kubectl: unexpected can-i:\$ARGV" >&2; exit 2 ;; esac ;;
  "api-resources --api-group=security.openshift.io") [ -n "\${STUB_OPENSHIFT:-}" ] && echo "securitycontextconstraints scc security.openshift.io/v1 false SecurityContextConstraints"; : ;;
  "delete serviceaccount") want -n check-ns; echo deleted ;;
  "apply -f")    cat > "\${STUB_APPLIED:-/dev/null}"; echo applied ;;
  "delete daemonset"|"delete pvc") want -n check-ns; echo deleted ;;
  "get pv")      want -l "$LBL"; want -o json; refuse -n; cat "$T/pvs.json" ;;
  "delete pv")   refuse -n; echo deleted ;;
  *) echo "stub kubectl: unexpected \$*" >&2; exit 2 ;;
esac
EOF
chmod +x "$T/bin/kubectl"
STUB_PATH="$T/bin:$PATH"
# the volumes carrying our labels: ours, the same model in another namespace,
# and one whose claimRef is not a weights claim at all
cat > "$T/pvs.json" <<EOF
{"items":[
 {"metadata":{"name":"${PVNAME}"},              "spec":{"claimRef":{"namespace":"check-ns","name":"${NAME}"}}},
 {"metadata":{"name":"${NAME}-othernamespace"}, "spec":{"claimRef":{"namespace":"other-ns","name":"${NAME}"}}},
 {"metadata":{"name":"weights-second-model-11111111-22222222"}, "spec":{"claimRef":{"namespace":"check-ns","name":"weights-second-model-11111111"}}}
]}
EOF

set +e
PATH="$STUB_PATH" bash deploy/weights.sh status -n check-ns > "$T/status.out" 2>&1
rc=$?
set -e
expect_line() { if grep -qE "^  $1 +$2 " "$T/status.out"; then ok "status: $1 -> $2"; else fail "status: $1 expected $2; got: $(grep -E "^  $1 " "$T/status.out" || echo none)"; fi; }
expect_line node-ready   present
grep -qE '^  node-ready +present +Ready *$' "$T/status.out" && ok "status: a healthy row ends at its phase (an empty reason is not swallowed, shifting the pod name into it)" || fail "status: healthy row: $(grep '^  node-ready' "$T/status.out")"
expect_line node-loading downloading
expect_line node-failing absent
grep -qF 'node-failing                 absent       Running (not ready) CrashLoopBackOff' "$T/status.out" && ok "status: a CrashLoopBackOff downloader is not 'downloading'" || fail "status: failing line: $(grep node-failing "$T/status.out")"
expect_line node-nopod   absent
expect_line node-evicted absent
expect_line node-unsched absent
grep -qE '^  node-unsched +absent +Pending \(not ready\)' "$T/status.out" && ok "status: a pod the DaemonSet made for a node but that never scheduled (no nodeName) shows on that node as Pending, not as no pod" || fail "status: unscheduled pod: $(grep '^  node-unsched' "$T/status.out")"
grep -q 'node-oom .*CrashLoopBackOff (last exit 137 OOMKilled)' "$T/status.out" && ok "status: a crash-looping downloader carries its last exit (an OOMKilled shows for a sub-second window otherwise)" || fail "status: last exit: $(grep '^  node-oom' "$T/status.out")"
grep -q 'node-evicted .*Evicted' "$T/status.out" && ok "status: an evicted downloader shows its reason" || fail "status: Evicted reason missing"
grep -q '^  node-cpu ' "$T/status.out" && fail "status: a node with an empty product label was counted" || ok "status: the default placement leaves the empty-label node out"
grep -q 'node-failing .*CrashLoopBackOff' "$T/status.out" && ok "status: a failing downloader shows its reason" || fail "status: reason missing"
grep -q 'Permission denied' "$T/status.out" && fail "status: a permission diagnosis with no permission error in the log" || ok "status: no permission diagnosis when the log shows none"
STUB_LOG='PermissionError: [Errno 13] Permission denied: /weights/models/Qwen' PATH="$STUB_PATH" bash deploy/weights.sh status -n check-ns > "$T/eacces.out" 2>&1 || true
grep -q 'cannot write .*Permission denied in its log' "$T/eacces.out" && grep -q 'chgrp 0 DIR && chmod 2775 DIR && chcon -t container_file_t DIR' "$T/eacces.out" && ok "status: a downloader dying on Permission denied is named, with the node-directory steps (OpenShift: project UID, no relabel)" || fail "status EACCES diagnosis: $(grep -c 'Permission' "$T/eacces.out") line(s)"
grep -q '^logs -n check-ns dl-failing --tail=20$' "$T/calls" && ok "status: the log read names the failing pod, no other" || fail "status: log read: $(grep '^logs' "$T/calls" | head -2 | tr '\n' ';')"
grep -q '1/8 usable nodes hold it; 7 do not' "$T/status.out" && ok "status: the tally counts only Ready (download complete) as holding, on usable nodes" || fail "status: tally: $(grep 'nodes hold' "$T/status.out")"
expect_line node-cordoned excluded
grep -qE '^  node-foreign +excluded +tainted example.com/team-x:NoSchedule' "$T/status.out" && ok "status: a node tainted against the downloader is excluded, with the taint" || fail "status: foreign taint: $(grep '^  node-foreign' "$T/status.out")"
grep -q '2 node(s) excluded and not counted' "$T/status.out" && ok "status: the excluded nodes are counted out, and said so" || fail "status: excluded note: $(grep excluded "$T/status.out" | tail -1)"
# A finished preparation exits 0 although the cluster has a cordoned node
# under DiskPressure and a node tainted for another team: neither can run an
# engine, and counting them reported 8/17 and failed a complete preparation.
cp "$T/nodes.json" "$T/nodes-full.json"
jq '.items |= map(select(.metadata.name == "node-ready" or .metadata.name == "node-cordoned" or .metadata.name == "node-foreign"))' "$T/nodes-full.json" > "$T/nodes.json"
if PATH="$STUB_PATH" bash deploy/weights.sh status -n check-ns > "$T/done.out" 2>&1; then grep -q '1/1 usable nodes hold it' "$T/done.out" && ok "status: every usable node holding the weights is a pass, cordoned and foreign-tainted nodes aside" || fail "status complete: $(grep 'nodes hold' "$T/done.out")"; else fail "status: a complete preparation failed on nodes no engine can use: $(grep -E 'nodes hold|excluded' "$T/done.out" | tr '
' ';')"; fi
cp "$T/nodes-full.json" "$T/nodes.json"
grep -q '1 node(s) carry an AMD, Intel or Gaudi accelerator label' "$T/status.out" && ok "status: a non-NVIDIA node under the default placement is named" || fail "status: vendor warning missing"
grep -q "claim ${NAME}: Bound" "$T/status.out" && ok "status: reports the claim Bound" || fail "status: claim line: $(head -1 "$T/status.out")"
[ "$rc" -ne 0 ] && ok "status: exits non-zero while a node lacks the weights" || fail "status: exit 0 with nodes lacking the weights"
grep -q 'stub kubectl:' "$T/status.out" && fail "status: a kubectl call lacked its scope: $(grep 'stub kubectl:' "$T/status.out" | head -1)" || ok "status: every kubectl call carried -n / -l"
grep -q "^Qwen/Qwen3-32B  (claim" "$T/status.out" && ok "status: with no --model the models are discovered from the DaemonSets" || fail "status: discovery: $(head -1 "$T/status.out")"
: > "$T/calls"
PATH="$STUB_PATH" bash deploy/weights.sh status -n check-ns --model Qwen/Qwen3-32B > "$T/explicit.out" 2>&1 || true
grep -q '^  node-ready ' "$T/explicit.out" && ! grep -q "^get daemonset -n check-ns -l" "$T/calls" && ok "status --model: reports without discovering the DaemonSets" || fail "status --model: $(head -2 "$T/explicit.out"); calls: $(grep '^get daemonset' "$T/calls" | head -2 | tr '\n' ';')"
if PATH="$STUB_PATH" bash deploy/weights.sh status -n check-ns --model 'a/b/c' >/dev/null 2>&1; then fail "status --model must refuse a bad model id"; else ok "status --model: a bad model id is refused"; fi
# no holder anywhere: the DaemonSet's FailedCreate event
cp "$T/pods.json" "$T/pods-all.json"; printf '{"items":[]}' > "$T/pods.json"
if STUB_EVENT='pods "weights-x-" is forbidden: violates PodSecurity "restricted:latest"' PATH="$STUB_PATH" bash deploy/weights.sh status -n check-ns > "$T/nopods.out" 2>&1; then fail "status with no downloader must fail"; else
    grep -q 'cannot create its pods: pods "weights-x-" is forbidden' "$T/nopods.out" && ok "status: with no downloader anywhere the DaemonSet's FailedCreate event is printed" || fail "status: FailedCreate event missing: $(tail -2 "$T/nopods.out")"; fi
cp "$T/pods-all.json" "$T/pods.json"
# no jq: apply still succeeds with the report skipped; status refuses
mkdir -p "$T/nojq"
for tool in bash sed awk tr printf cat grep head cut mktemp rm sort uniq shasum sha256sum env dirname basename readlink date id uname wc true false; do
    tp="$(command -v "$tool" 2>/dev/null || true)"; [ -n "$tp" ] && ln -sf "$tp" "$T/nojq/$tool"
done
if out="$(PATH="$T/bin:$T/nojq" bash deploy/weights.sh apply -n check-ns --model Qwen/Qwen3-32B --path /mnt/local/models --image "$IMG" 2>&1)"; then
    case "$out" in *"jq is not installed"*) ok "apply: without jq the apply succeeds and the report is skipped with a warning" ;; *) fail "apply without jq: $out" ;; esac
else
    fail "apply must not fail for a missing jq after the manifests were applied: $out"
fi
if PATH="$T/bin:$T/nojq" bash deploy/weights.sh status -n check-ns >/dev/null 2>&1; then fail "status without jq must fail"; else ok "status: without jq refuses with a reason"; fi
STUB_OPENSHIFT=1 PATH="$STUB_PATH" bash deploy/weights.sh apply -n check-ns --model Qwen/Qwen3-32B --path /var/mnt/weights --image "$IMG" > "$T/ocp.out" 2>&1 || true
grep -q 'OpenShift: the downloader runs as the project UID' "$T/ocp.out" && grep -q 'chcon -t container_file_t /var/mnt/weights' "$T/ocp.out" && ok "apply: on a cluster with SecurityContextConstraints the OpenShift node-directory steps are printed once, with the path" || fail "apply on OpenShift: $(grep -c OpenShift "$T/ocp.out") notice(s)"
PATH="$STUB_PATH" bash deploy/weights.sh apply -n check-ns --model Qwen/Qwen3-32B --path /mnt/local/models --image "$IMG" > "$T/k8s.out" 2>&1 || true
grep -q 'OpenShift:' "$T/k8s.out" && fail "apply: the OpenShift notice printed on a cluster without SCCs" || ok "apply: no OpenShift notice on a cluster without SecurityContextConstraints"
if STUB_PVC_PHASE=Pending PATH="$STUB_PATH" bash deploy/weights.sh status -n check-ns > "$T/pending.out" 2>&1; then fail "status must fail while the claim is not Bound"; else grep -q 'Pending' "$T/pending.out" && grep -q 'the claim is not bound: its volume is cluster-scoped and was not created' "$T/pending.out" && ok "status: an unbound claim is reported with why (the volume needs PersistentVolume create) and fails" || fail "status: unbound claim: $(head -2 "$T/pending.out" | tr '\n' ';')"; fi
# apply refuses before anything lands when the volume cannot be created
: > "$T/calls"
if STUB_NO_PV=1 PATH="$STUB_PATH" bash deploy/weights.sh apply -n check-ns --model Qwen/Qwen3-32B --path /mnt/local/models --image "$IMG" > "$T/nopv.out" 2>&1; then fail "apply must refuse without leave to create PersistentVolumes"; else
    grep -q 'leave to create PersistentVolumes' "$T/nopv.out" && ! grep -q '^apply -f' "$T/calls" && ok "apply: without leave to create PersistentVolumes nothing is applied, and the reason names the cluster admin" || fail "apply without PV rights: $(tail -1 "$T/nopv.out"); calls: $(grep '^apply' "$T/calls")"; fi
# A re-apply keeps an existing claim's size. The claim is bound to a static
# volume and only a dynamically provisioned claim can be resized, so a
# re-apply rendering the default failed mid-apply on every re-run once the
# default changed. --capacity equal to the claim is fine (in any spelling of
# the size); a different one is refused before anything lands; a claim that
# cannot be read is an error, not an absent claim.
claim_size() { yq -r 'select(.kind == "PersistentVolumeClaim") | .spec.resources.requests.storage' "$1" 2>/dev/null; }
pv_size() { yq -r 'select(.kind == "PersistentVolume") | .spec.capacity.storage' "$1" 2>/dev/null; }
WAPPLY=(apply -n check-ns --model Qwen/Qwen3-32B --path /mnt/local/models --image "$IMG")
: > "$T/applied.yaml"
STUB_PVC_SIZE=200Gi STUB_APPLIED="$T/applied.yaml" PATH="$STUB_PATH" bash deploy/weights.sh "${WAPPLY[@]}" > "$T/keep.out" 2>&1 || true
[ "$(claim_size "$T/applied.yaml")" = 200Gi ] && [ "$(pv_size "$T/applied.yaml")" = 200Gi ] && grep -q 'exists at 200Gi; keeping that size' "$T/keep.out" \
    && ok "re-apply: an existing 200Gi claim and no --capacity renders 200Gi on the claim and the volume, and says so" || fail "re-apply keep: claim '$(claim_size "$T/applied.yaml")' volume '$(pv_size "$T/applied.yaml")'; $(head -2 "$T/keep.out" | tr '\n' ';')"
: > "$T/applied.yaml"
STUB_PVC_SIZE=200Gi STUB_APPLIED="$T/applied.yaml" PATH="$STUB_PATH" bash deploy/weights.sh "${WAPPLY[@]}" --capacity 200Gi > "$T/equal.out" 2>&1 || true
[ "$(claim_size "$T/applied.yaml")" = 200Gi ] && ! grep -q 'cannot be resized' "$T/equal.out" && ok "re-apply: --capacity equal to the existing claim applies" || fail "re-apply equal: '$(claim_size "$T/applied.yaml")'; $(head -2 "$T/equal.out" | tr '\n' ';')"
: > "$T/applied.yaml"
STUB_PVC_SIZE=200Gi STUB_APPLIED="$T/applied.yaml" PATH="$STUB_PATH" bash deploy/weights.sh "${WAPPLY[@]}" --capacity 204800Mi > "$T/equal2.out" 2>&1 || true
[ "$(claim_size "$T/applied.yaml")" = 200Gi ] && ok "re-apply: --capacity of the same size written otherwise (204800Mi for 200Gi) applies, as the claim has it" || fail "re-apply same size: '$(claim_size "$T/applied.yaml")'; $(head -2 "$T/equal2.out" | tr '\n' ';')"
: > "$T/calls"
if STUB_PVC_SIZE=200Gi PATH="$STUB_PATH" bash deploy/weights.sh "${WAPPLY[@]}" --capacity 1Ti > "$T/resize.out" 2>&1; then fail "re-apply must refuse a --capacity that differs from the existing claim"; else
    grep -q -- '--capacity 1Ti differs from the existing claim .* which is 200Gi: a claim bound to a static (node-local) volume cannot be resized' "$T/resize.out" && grep -q 'weights.sh delete -n check-ns --model Qwen/Qwen3-32B' "$T/resize.out" && ! grep -q '^apply -f' "$T/calls" \
        && ok "re-apply: a different --capacity is refused before anything lands, saying why and what frees the claim" || fail "re-apply resize: $(tail -1 "$T/resize.out"); calls: $(grep '^apply' "$T/calls")"; fi
# the delete it names frees nothing while a pod mounts the claim: the
# refusal says to scale the engines down first
grep -q 'scale the model servers (engines) using .* to 0 first, or pvc-protection leaves it Terminating' "$T/resize.out" \
    && ok "re-apply resize: the refusal says no pod may mount the claim (scale the engines to 0) or it stays Terminating" || fail "re-apply resize: no word on pods mounting the claim: $(tail -1 "$T/resize.out")"
# A decimal size or an exponent is the same size as its integer spelling:
# 1.5Ti against a claim stored as 1536Gi was compared as text and refused.
: > "$T/applied.yaml"
STUB_PVC_SIZE=1536Gi STUB_APPLIED="$T/applied.yaml" PATH="$STUB_PATH" bash deploy/weights.sh "${WAPPLY[@]}" --capacity 1.5Ti > "$T/equal3.out" 2>&1 || true
[ "$(claim_size "$T/applied.yaml")" = 1536Gi ] && ok "re-apply: --capacity 1.5Ti against a 1536Gi claim applies, as the claim has it" || fail "re-apply 1.5Ti vs 1536Gi: '$(claim_size "$T/applied.yaml")'; $(head -2 "$T/equal3.out" | tr '\n' ';')"
qbad=0
for pair in '1.5Ti 1536Gi' '0.5Gi 512Mi' '.5Ki 512' '1e3 1k' '1E3 1k' '1.0Ti 1Ti' '2.5k 2500' '1e0 1' '1000e-3 1'; do
    # shellcheck disable=SC2086
    bash -c 'source deploy/lib/nodedir.sh; nodedir_same_quantity "$1" "$2"' _ $pair || { fail "nodedir_same_quantity: '${pair% *}' and '${pair#* }' are the same size"; qbad=1; }
done
for pair in '1.5Ti 1Ti' '1e3 1Ki' '0.5Gi 500Mi' '1.5Ti 1537Gi'; do
    # shellcheck disable=SC2086
    if bash -c 'source deploy/lib/nodedir.sh; nodedir_same_quantity "$1" "$2"' _ $pair; then fail "nodedir_same_quantity: '${pair% *}' and '${pair#* }' differ"; qbad=1; fi
done
# a fraction of a byte, or past 2^63, is no claim size: not parsed
for q in 0.1 1.5 1e-1 16Ei 9999999999999999999 1e19 . ''; do
    if bash -c 'source deploy/lib/nodedir.sh; nodedir_quantity_bytes "$1" >/dev/null' _ "$q"; then fail "nodedir_quantity_bytes: '$q' must not parse to whole bytes"; qbad=1; fi
done
[ "$(bash -c 'source deploy/lib/nodedir.sh; nodedir_quantity_bytes 1.5Ti')" = 1649267441664 ] || { fail "nodedir_quantity_bytes 1.5Ti: '$(bash -c 'source deploy/lib/nodedir.sh; nodedir_quantity_bytes 1.5Ti')'"; qbad=1; }
[ "$qbad" -eq 0 ] && ok "sizes compare in bytes across decimals and exponents (1.5Ti = 1536Gi, 0.5Gi = 512Mi, 1e3 = 1k); a fraction of a byte or an overflow does not parse"
# A claim of the model's name on another storage class is not this
# script's: refused before anything lands, and not offered for deletion.
for extra in "" "--capacity 1Ti"; do
    : > "$T/calls"
    # shellcheck disable=SC2086
    if STUB_PVC_CLASS=standard PATH="$STUB_PATH" bash deploy/weights.sh "${WAPPLY[@]}" $extra > "$T/foreign.out" 2>&1; then fail "apply must refuse a claim of the model's name on another storage class (${extra:-no --capacity})"; else
        grep -q "already has a claim named ${NAME} on storage class 'standard', which weights.sh did not make" "$T/foreign.out" && ! grep -qi 'delete' "$T/foreign.out" && ! grep -q '^apply -f' "$T/calls" \
            && ok "apply: a claim of the model's name on another class is refused, named, not offered for deletion, nothing applied (${extra:-no --capacity})" || fail "apply on a foreign claim (${extra:-no --capacity}): $(tail -1 "$T/foreign.out"); calls: $(grep '^apply' "$T/calls")"; fi
done
: > "$T/applied.yaml"
STUB_PVC_PHASE="" STUB_APPLIED="$T/applied.yaml" PATH="$STUB_PATH" bash deploy/weights.sh "${WAPPLY[@]}" > "$T/fresh.out" 2>&1 || true
[ "$(claim_size "$T/applied.yaml")" = 1Ti ] && ! grep -q 'keeping that size' "$T/fresh.out" && ok "apply: no claim yet renders the default 1Ti" || fail "apply fresh: '$(claim_size "$T/applied.yaml")'"
: > "$T/calls"
if STUB_PVC_READ_ERROR='Error from server (Forbidden): persistentvolumeclaims is forbidden: stub' PATH="$STUB_PATH" bash deploy/weights.sh "${WAPPLY[@]}" > "$T/pvcforb.out" 2>&1; then fail "apply must fail when the claim cannot be read"; else
    grep -q 'cannot read the claim .*(kubectl: Error from server (Forbidden): persistentvolumeclaims is forbidden: stub)' "$T/pvcforb.out" && ! grep -q '^apply -f' "$T/calls" \
        && ok "apply: a claim that cannot be read (Forbidden) is an error with kubectl's reason, not an absent claim; nothing applied" || fail "apply pvc Forbidden: $(tail -1 "$T/pvcforb.out"); calls: $(grep '^apply' "$T/calls")"; fi
if STUB_PVC_READ_ERROR='stub: must not be read' PATH="$STUB_PATH" bash deploy/weights.sh "${WAPPLY[@]}" --dry-run > "$T/dry.out" 2>&1 && [ "$(claim_size "$T/dry.out")" = 1Ti ]; then ok "apply --dry-run reads no claim and renders the default"; else fail "apply --dry-run: $(tail -1 "$T/dry.out")"; fi
if STUB_DAEMONSETS="" PATH="$STUB_PATH" bash deploy/weights.sh status -n check-ns > "$T/none.out" 2>&1; then fail "status with nothing to check must fail"; else grep -q 'no weights DaemonSets' "$T/none.out" && ok "status: no DaemonSets and no --model is refused with a reason" || fail "status: $(tail -1 "$T/none.out")"; fi
: > "$T/calls"
STUB_DS_SELECTOR=example.com/accelerator=h200 STUB_SELECTOR=example.com/accelerator=h200 PATH="$STUB_PATH" bash deploy/weights.sh status -n check-ns >/dev/null 2>&1 || true
grep -q 'get nodes -l example.com/accelerator=h200' "$T/calls" && ok "status: lists the nodes the DaemonSet was applied for (its live nodeSelector)" || fail "status: live selector not used: $(grep 'get nodes' "$T/calls")"
if STUB_DS_SELECTOR='a=b"c' PATH="$STUB_PATH" bash deploy/weights.sh status -n check-ns > "$T/edited.out" 2>&1; then fail "status must refuse a DaemonSet nodeSelector that is not label characters"; else
    grep -q 'this script did not write' "$T/edited.out" && ok "status: a DaemonSet nodeSelector this script did not write is refused before kubectl" || fail "status on an edited DaemonSet: $(tail -1 "$T/edited.out")"; fi
# a live nodeSelector of two keys reads back as both: listing the nodes on
# the first alone would count nodes the DaemonSet is never scheduled to
: > "$T/calls"
STUB_DS_SELECTOR="$SEL2" STUB_SELECTOR="$SEL2" PATH="$STUB_PATH" bash deploy/weights.sh status -n check-ns > "$T/sel2-live.out" 2>&1 || true
grep -q "get nodes -l ${SEL2} " "$T/calls" && ok "status: every pair of the live nodeSelector reaches the node listing" || fail "status: two-key live selector read back as: $(grep 'get nodes' "$T/calls"); $(tail -1 "$T/sel2-live.out")"
# a node matches only when it carries every pair
cat > "$T/nodes-sel2.json" <<'EOF'
{"items":[
 {"metadata":{"name":"node-both",        "labels":{"example.com/accelerator":"h200","example.com/state":"production"}}},
 {"metadata":{"name":"node-accel-only",  "labels":{"example.com/accelerator":"h200"}}},
 {"metadata":{"name":"node-state-only",  "labels":{"example.com/state":"production"}}},
 {"metadata":{"name":"node-other-state", "labels":{"example.com/accelerator":"h200","example.com/state":"broken"}}},
 {"metadata":{"name":"node-both-cordoned","labels":{"example.com/accelerator":"h200","example.com/state":"production"}},"spec":{"unschedulable":true}},
 {"metadata":{"name":"node-accel-cordoned","labels":{"example.com/accelerator":"h200"}},"spec":{"unschedulable":true}}
]}
EOF
STUB_NODES_FILE="$T/nodes-sel2.json" STUB_SELECTOR="$SEL2" PATH="$STUB_PATH" bash deploy/weights.sh status -n check-ns --node-selector "$SEL2" > "$T/sel2-status.out" 2>&1 || true
grep -q '^  node-both ' "$T/sel2-status.out" && ! grep -Eq 'node-(accel-only|state-only|other-state|accel-cordoned)' "$T/sel2-status.out" \
    && ok "status: a node carrying only one of the pairs is not counted" || fail "status two-pair node rows: $(grep -o 'node-[a-z-]*' "$T/sel2-status.out" | sort -u | tr '\n' ' ')"
if STUB_NODES_ERROR='error: unable to parse requirement' PATH="$STUB_PATH" bash deploy/weights.sh status -n check-ns > "$T/nf.out" 2>&1; then fail "status must fail when nodes cannot be listed"; else
    grep -q 'unable to parse requirement' "$T/nf.out" && ! grep -q cluster-reader "$T/nf.out" && ok "status: a node-list failure that is not a Forbidden reports kubectl's reason" || fail "status non-Forbidden: $(tail -1 "$T/nf.out")"; fi

# delete: DaemonSet and claim in the namespace; the volume found by OUR
# labels and its claimRef into this namespace -- never by a name read off a
# claim, which a tenant writes
: > "$T/calls"
PATH="$STUB_PATH" bash deploy/weights.sh delete -n check-ns --model Qwen/Qwen3-32B >/dev/null 2>&1
grep -q "^delete daemonset -n check-ns ${NAME} --ignore-not-found" "$T/calls" && grep -q "^delete pvc -n check-ns ${NAME} --ignore-not-found" "$T/calls" \
    && grep -q "^delete pv ${PVNAME} --ignore-not-found" "$T/calls" && ok "delete --model: DaemonSet, claim, and the volume whose claimRef is this claim" || fail "delete --model issued: $(grep delete "$T/calls" | tr '\n' ';')"
grep -q '^get pvc' "$T/calls" && fail "delete: a claim was read for its volume name (a tenant writes that field)" || ok "delete: no claim is read for a volume name"
grep -q "^delete pv .*othernamespace" "$T/calls" && fail "delete --model: another namespace's volume for the same model was deleted" || ok "delete --model: another namespace's volume for the same model is left alone"
grep -q "^delete pv .*second-model" "$T/calls" && fail "delete --model: another model's volume was deleted" || ok "delete --model: another model's volume in this namespace is left alone"
grep -q "^delete serviceaccount" "$T/calls" && fail "delete --model: the ServiceAccount other models' downloaders share was deleted" || ok "delete --model: the shared ServiceAccount stays"
: > "$T/calls"
PATH="$STUB_PATH" bash deploy/weights.sh delete -n check-ns --all >/dev/null 2>&1
grep -q "^delete daemonset -n check-ns -l ${LBL} --ignore-not-found" "$T/calls" && grep -q "^delete pvc -n check-ns -l ${LBL} --ignore-not-found" "$T/calls" && ok "delete --all: by both labels" || fail "delete --all issued: $(grep delete "$T/calls" | tr '\n' ';')"
line="$(grep '^delete pv ' "$T/calls" || true)"
case "$line" in
    "delete pv ${PVNAME} weights-second-model-11111111-22222222 --ignore-not-found"|"delete pv weights-second-model-11111111-22222222 ${PVNAME} --ignore-not-found") ok "delete --all: every volume with our labels whose claimRef is in this namespace, and no other" ;;
    *) fail "delete --all pv delete: $line" ;;
esac
grep -q "^get pv -l ${LBL} -o json" "$T/calls" && ok "delete: the volumes are listed by our labels, cluster-scoped" || fail "delete: pv listing: $(grep '^get pv' "$T/calls")"
grep -q "^delete serviceaccount -n check-ns weights-downloader --ignore-not-found" "$T/calls" && ok "delete --all: the downloader ServiceAccount goes too" || fail "delete --all: ServiceAccount kept: $(grep serviceaccount "$T/calls")"
: > "$T/calls"
printf '{"items":[]}' > "$T/pvs-none.json"; cp "$T/pvs.json" "$T/pvs-all.json"; cp "$T/pvs-none.json" "$T/pvs.json"
PATH="$STUB_PATH" bash deploy/weights.sh delete -n check-ns --all >/dev/null 2>&1
grep -q '^delete pv ' "$T/calls" && fail "delete: with no volume of ours there is nothing to delete" || ok "delete --all with no volume of ours issues no pv delete"
cp "$T/pvs-all.json" "$T/pvs.json"
: > "$T/calls"
out="$(PATH="$STUB_PATH" bash deploy/weights.sh delete -n check-ns --all --dry-run 2>&1 || true)"
grep -q '^delete' "$T/calls" && fail "delete --dry-run deleted: $(grep delete "$T/calls")" || { case "$out" in *"would run: kubectl delete daemonset"*) ok "delete --dry-run prints and deletes nothing" ;; *) fail "delete --dry-run: $out" ;; esac; }

# ---------------------------------------------------------------------------
# 3. model_hostpath.sh: both scenario layouts, the guard, the placement.
# ---------------------------------------------------------------------------
cat > "$T/nested.yaml" <<'EOF'
# a scenario that keeps storage under scenario[].common
scenario:
  - name: one
    common:
      model:
        path: models/Qwen/Qwen3-0.6B
      storage:
        modelPvc:
          size: 40Gi   # the claim
          accessModes: [ReadWriteMany]
    harness:
      name: x
EOF
cat > "$T/shared.yaml" <<'EOF'
shared:
  storage:
    modelPvc:
      size: 32Gi
scenario:
  - name: a
    model: {path: models/a}
  - name: b
    model: {path: models/b}
EOF
cp "$T/nested.yaml" "$T/n1.yaml"
PATH="$STUB_PATH" bash hack/benchmark/model_hostpath.sh "$T/n1.yaml" /mnt/local/scaling-manager check-ns > "$T/hp1.out" 2>&1 || fail "model_hostpath on the nested layout failed: $(cat "$T/hp1.out")"
"$PY" - "$T/n1.yaml" deploy/lib/accelerator_labels.py <<'PYEOF' || FAILED=1
import sys, yaml
d = yaml.safe_load(open(sys.argv[1]))
ns = {}; exec(open(sys.argv[2]).read(), ns)
hp = d["scenario"][0]["common"]["storage"]["hostPath"]
bad = []
def check(c, m):
    if not c: bad.append(m)
check(hp["enabled"] is True and hp["path"] == "/mnt/local/scaling-manager", "enabled with the path: %s" % hp)
check(hp["capacity"] == "40Gi", "capacity is the claim's size (a larger request never binds): %s" % hp.get("capacity"))
check(hp["storageClassName"] == "node-local-weights", "storage class")
check("nodeSelector" not in hp, "no selector given: no nodeSelector")
terms = hp["affinity"]["nodeAffinity"]["requiredDuringSchedulingIgnoredDuringExecution"]["nodeSelectorTerms"]
check([t["matchExpressions"][0]["key"] for t in terms] == ns["PRODUCT_KEYS"], "the affinity is the product-key list")
check(all(t["matchExpressions"][1] == {"key": t["matchExpressions"][0]["key"], "operator": "NotIn", "values": [""]} for t in terms), "each term requires a non-empty label")
check(hp["tolerations"] == [{"key": "nvidia.com/gpu", "operator": "Exists"}], "tolerates the accelerator taint")
check(d["scenario"][0]["common"]["storage"]["modelPvc"]["size"] == "40Gi" and d["scenario"][0]["harness"] == {"name": "x"}, "nothing else changed")
# The harness waits until numberReady equals desired; a cordoned node and a node
# tainted for another team are in desired unless the affinity keeps them out.
notin = {"key": "kubernetes.io/hostname", "operator": "NotIn", "values": ["node-cordoned", "node-foreign"]}
check(all(len(t["matchExpressions"]) == 3 and t["matchExpressions"][2] == notin for t in terms), "every term keeps the downloader off the cordoned and the foreign-tainted node: %s" % [t["matchExpressions"][2:] for t in terms])
if bad:
    [print("  FAIL " + b) for b in bad]; sys.exit(1)
print("  ok   model_hostpath: nested layout gets the block under common.storage with the affinity and the claim's size")
print("  ok   model_hostpath: the downloader is kept off a cordoned node and a node tainted against it, so the harness wait can complete")
PYEOF
grep -q 'kept off node-cordoned (cordoned), node-foreign (tainted example.com/team-x:NoSchedule)' "$T/hp1.out" && ok "model_hostpath: says which nodes it kept the downloader off, and why" || fail "model_hostpath exclusion message: $(cat "$T/hp1.out")"
# a taint the run tolerates is not a reason to exclude
cp "$T/nested.yaml" "$T/n8.yaml"
PATH="$STUB_PATH" bash hack/benchmark/model_hostpath.sh "$T/n8.yaml" /mnt/local/scaling-manager check-ns "" example.com/team-x > "$T/hp8.out" 2>&1 || fail "model_hostpath with a toleration failed: $(cat "$T/hp8.out")"
[ "$(yq -r '.scenario[0].common.storage.hostPath.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[2].values | join(",")' "$T/n8.yaml")" = node-cordoned ] && ok "model_hostpath: a node whose taint the run tolerates stays in; the cordoned one does not" || fail "model_hostpath tolerated taint: $(yq -o json -I0 '.scenario[0].common.storage.hostPath.affinity' "$T/n8.yaml")"
# with a KEY=VALUE selector the exclusion is an affinity beside the nodeSelector
cp "$T/nested.yaml" "$T/n9.yaml"
STUB_SELECTOR=example.com/accelerator=h200 PATH="$STUB_PATH" bash hack/benchmark/model_hostpath.sh "$T/n9.yaml" /mnt/local/scaling-manager check-ns example.com/accelerator=h200 > "$T/hp9.out" 2>&1 || fail "model_hostpath with a selector failed: $(cat "$T/hp9.out")"
[ "$(yq -r '.scenario[0].common.storage.hostPath | .nodeSelector["example.com/accelerator"] + " " + (.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].values | join(","))' "$T/n9.yaml")" = "h200 node-cordoned,node-foreign" ] && ok "model_hostpath: with a selector, the excluded nodes are an affinity beside the nodeSelector" || fail "model_hostpath selector exclusion: $(yq -o json -I0 '.scenario[0].common.storage.hostPath' "$T/n9.yaml")"
# two pairs: both are nodeSelector entries, and the nodes kept off are only
# those carrying both (a cordoned node with one of the labels is not a node
# the downloader would go to, so it is not named)
cp "$T/nested.yaml" "$T/n10.yaml"
if STUB_NODES_FILE="$T/nodes-sel2.json" STUB_SELECTOR="$SEL2" PATH="$STUB_PATH" bash hack/benchmark/model_hostpath.sh "$T/n10.yaml" /mnt/local/scaling-manager check-ns "$SEL2" > "$T/hp10.out" 2>&1; then
    [ "$(yq -o json -I0 '.scenario[0].common.storage.hostPath.nodeSelector' "$T/n10.yaml")" = '{"example.com/accelerator":"h200","example.com/state":"production"}' ] \
        && ok "model_hostpath: two pairs become a nodeSelector with both keys" || fail "model_hostpath two pairs: $(yq -o json -I0 '.scenario[0].common.storage.hostPath' "$T/n10.yaml")"
    [ "$(yq -r '.scenario[0].common.storage.hostPath.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].values | join(",")' "$T/n10.yaml")" = node-both-cordoned ] \
        && ok "model_hostpath: with two pairs only a node carrying both is a candidate to keep off" || fail "model_hostpath two-pair exclusion: $(yq -o json -I0 '.scenario[0].common.storage.hostPath.affinity' "$T/n10.yaml")"
    grep -q 'on nodes with example.com/accelerator=h200 and example.com/state=production;' "$T/hp10.out" && ok "model_hostpath: the message says both pairs must match" || fail "model_hostpath two-pair message: $(tail -1 "$T/hp10.out")"
else
    fail "model_hostpath refused a two-pair selector: $(cat "$T/hp10.out")"
fi
selbad=0
for badsel in 'a=b,' 'a=b,,c=d' 'a=b,a=c' 'a=b c' $'a=b\nc=d' 'a/b/c=d' 'a=-b'; do
    cp "$T/nested.yaml" "$T/n11.yaml"
    if PATH="$STUB_PATH" bash hack/benchmark/model_hostpath.sh "$T/n11.yaml" /mnt/local/scaling-manager check-ns "$badsel" >/dev/null 2>&1; then fail "model_hostpath accepted the selector '$badsel'"; selbad=1; fi
done
[ "$selbad" -eq 0 ] && ok "model_hostpath: a trailing comma, an empty pair, a key twice, a space, a newline or a key or value outside label syntax is refused"
# A node listing that fails says why: kubectl's own error is in the message
# (a selector the API server refuses reads as that, not as a bare "cannot
# list"), and it is never taken for the node list.
cp "$T/nested.yaml" "$T/n12.yaml"
STUB_NODES_ERROR='error: unable to parse requirement: stub refusal' STUB_SELECTOR=example.com/accelerator=h200 PATH="$STUB_PATH" \
    bash hack/benchmark/model_hostpath.sh "$T/n12.yaml" /mnt/local/scaling-manager check-ns example.com/accelerator=h200 > "$T/hp12.out" 2>&1 || fail "model_hostpath with a failed node listing failed: $(cat "$T/hp12.out")"
grep -qF 'cannot list nodes with selector example.com/accelerator=h200 (kubectl: error: unable to parse requirement: stub refusal)' "$T/hp12.out" \
    && [ "$(yq -r '.scenario[0].common.storage.hostPath.nodeSelector["example.com/accelerator"]' "$T/n12.yaml")" = h200 ] \
    && ok "model_hostpath: a failed node listing reports kubectl's error and the selector, and the edit still lands" || fail "model_hostpath node-listing error: $(cat "$T/hp12.out")"
grep -q '^#' "$T/n1.yaml" && ok "model_hostpath: the scenario keeps its comments (yq, not a dump)" || fail "model_hostpath: comments lost"
# The rule itself, on the cases the fixtures above do not reach: a toleration
# with no key tolerates every taint, and PreferNoSchedule never excludes.
printf '%s' '{"items":[{"metadata":{"name":"soft"},"spec":{"taints":[{"key":"example.com/x","effect":"PreferNoSchedule"}]}},{"metadata":{"name":"hard"},"spec":{"taints":[{"key":"example.com/x","effect":"NoSchedule"}]}}]}' > "$T/rule.json"
rule="$(bash -c '. deploy/lib/accelerator_nodes.sh; accelerator_excluded_json "$1" < "$2" | jq -r "map(.name) | join(\",\")"' _ '["nvidia.com/gpu"]' "$T/rule.json")"
any="$(bash -c '. deploy/lib/accelerator_nodes.sh; accelerator_excluded_json "$1" < "$2" | jq -r "length"' _ '["*"]' "$T/rule.json")"
[ "$rule" = hard ] && [ "$any" = 0 ] && ok "usable nodes: PreferNoSchedule never excludes, and a keyless toleration tolerates every taint" || fail "usable-node rule: excluded '$rule' with nvidia.com/gpu, $any with a keyless toleration"
cp "$T/shared.yaml" "$T/s1.yaml"
PATH="$STUB_PATH" bash hack/benchmark/model_hostpath.sh "$T/s1.yaml" /mnt/local/scaling-manager check-ns example.com/accelerator=h200 dedicated,example.com/pool > "$T/hp2.out" 2>&1 || fail "model_hostpath on the shared layout failed: $(cat "$T/hp2.out")"
[ "$(yq -r '.shared.storage.hostPath.tolerations | map(.key) | join(",")' "$T/s1.yaml")" = "nvidia.com/gpu,dedicated,example.com/pool" ] && ok "model_hostpath: the taint keys reach the harness DaemonSet's tolerations, after nvidia.com/gpu" || fail "model_hostpath tolerations: $(yq '.shared.storage.hostPath.tolerations' "$T/s1.yaml")"
cp "$T/nested.yaml" "$T/n7.yaml"
if PATH="$STUB_PATH" bash hack/benchmark/model_hostpath.sh "$T/n7.yaml" /mnt/local/scaling-manager check-ns "" 'bad key' >/dev/null 2>&1; then fail "model_hostpath accepted a taint key with a space"; else ok "model_hostpath: a taint key that is not one is refused"; fi
sel="$(yq -r '.shared.storage.hostPath.nodeSelector["example.com/accelerator"] // ""' "$T/s1.yaml")"
[ "$sel" = h200 ] && [ "$(yq -r '.shared.storage.hostPath.affinity // "none"' "$T/s1.yaml")" = none ] && ok "model_hostpath: shared layout, and a KEY=VALUE becomes a nodeSelector with no affinity" || fail "model_hostpath shared: $(yq '.shared.storage.hostPath' "$T/s1.yaml")"
[ "$(yq -r '.shared.storage.hostPath.capacity' "$T/s1.yaml")" = 32Gi ] && ok "model_hostpath: capacity follows the shared claim's size" || fail "model_hostpath: shared capacity"
# the guard: an existing claim on another class is refused before any edit
cp "$T/nested.yaml" "$T/n2.yaml"
if STUB_MODEL_PVC_CLASS=shared-vast PATH="$STUB_PATH" bash hack/benchmark/model_hostpath.sh "$T/n2.yaml" /mnt/local/scaling-manager check-ns > "$T/hp3.out" 2>&1; then fail "model_hostpath must refuse a namespace whose model-pvc is on another class"; else
    grep -q "already has model-pvc on storage class 'shared-vast'" "$T/hp3.out" && cmp -s "$T/nested.yaml" "$T/n2.yaml" && ok "model_hostpath: an existing model-pvc on another class is refused, scenario untouched" || fail "model_hostpath guard: $(tail -1 "$T/hp3.out")"; fi
cp "$T/nested.yaml" "$T/n3.yaml"
if STUB_MODEL_PVC_CLASS=node-local-weights PATH="$STUB_PATH" bash hack/benchmark/model_hostpath.sh "$T/n3.yaml" /mnt/local/scaling-manager check-ns >/dev/null 2>&1; then ok "model_hostpath: an existing claim already on the node-local class is fine (a re-standup)"; else fail "model_hostpath refused a claim on its own class"; fi
for badp in relative / '/a b'; do
    cp "$T/nested.yaml" "$T/n4.yaml"
    if PATH="$STUB_PATH" bash hack/benchmark/model_hostpath.sh "$T/n4.yaml" "$badp" check-ns >/dev/null 2>&1; then fail "model_hostpath accepted the path '$badp'"; fi
done
ok "model_hostpath: a relative, root or unusual path is refused"
cp "$T/nested.yaml" "$T/n5.yaml"
if PATH="$STUB_PATH" bash hack/benchmark/model_hostpath.sh "$T/n5.yaml" /mnt/local/scaling-manager check-ns 'a=b"' >/dev/null 2>&1; then fail "model_hostpath accepted a selector with a quote"; else ok "model_hostpath: a selector that is not one KEY=VALUE is refused"; fi
cat > "$T/two.yaml" <<'EOF'
scenario:
  - name: a
    common:
      storage:
        modelPvc:
          size: 40Gi
  - name: b
    common:
      storage:
        modelPvc:
          size: 80Gi
EOF
if PATH="$STUB_PATH" bash hack/benchmark/model_hostpath.sh "$T/two.yaml" /mnt/local/scaling-manager check-ns > "$T/two.out" 2>&1; then
    a="$(yq -r '.scenario[0].common.storage.hostPath.capacity' "$T/two.yaml")"; b="$(yq -r '.scenario[1].common.storage.hostPath.capacity' "$T/two.yaml")"
    [ "$a" = 40Gi ] && [ "$b" = 40Gi ] && grep -q '(2 storage block(s))' "$T/two.out" && ok "model_hostpath: two stacks both get the block, the first claim's size (one volume, one capacity), and the message counts two" || fail "model_hostpath two stacks: a=$a b=$b; $(tail -1 "$T/two.out")"
else
    fail "model_hostpath on two stacks failed: $(cat "$T/two.out")"
fi
printf 'scenario:\n  - name: x\n    common: {}\n' > "$T/n6.yaml"
if PATH="$STUB_PATH" bash hack/benchmark/model_hostpath.sh "$T/n6.yaml" /mnt/local/scaling-manager check-ns >/dev/null 2>&1; then fail "model_hostpath must fail on a scenario with no model claim"; else ok "model_hostpath: a scenario with no modelPvc is refused, not silently left on the shared volume"; fi

# ---------------------------------------------------------------------------
# 4. The Makefile: the targets' argv and the standup's step.
# ---------------------------------------------------------------------------
# the recipe is one command over two lines; join them before matching
line="$(make -n weights WEIGHTS_MODEL=Qwen/Qwen3-32B WEIGHTS_PATH=/mnt/local/w WEIGHTS_IMAGE=i:1 WEIGHTS_HF_TOKEN_SECRET=hf NAMESPACE=ns PREPULL_TOLERATIONS=t1 2>/dev/null | tr -d '\\\n' | grep -o 'bash deploy/weights.sh apply.*' || true)"
case "$line" in
    *'--model "Qwen/Qwen3-32B" --path "/mnt/local/w" --image "i:1"'*'--hf-token-secret "hf"'*'--toleration t1'*) ok "make weights: model, path, image, secret and the PREPULL_* tolerations reach the script" ;;
    *) fail "make weights expanded to: $line" ;;
esac
case "$line" in *--node-selector*) fail "make weights: an empty selector must pass no --node-selector" ;; *) ok "make weights: no selector means the product-key affinity" ;; esac
line="$(make -n weights WEIGHTS_MODEL=m WEIGHTS_PATH=/p WEIGHTS_IMAGE=i NAMESPACE=ns PREPULL_NODE_SELECTOR=k=v 2>/dev/null | tr -d '\\\n' || true)"
case "$line" in *'--node-selector "k=v"'*) ok "make weights: PREPULL_NODE_SELECTOR is inherited" ;; *) fail "make weights inheritance: $line" ;; esac
line="$(make -n weights WEIGHTS_MODEL=m WEIGHTS_PATH=/p WEIGHTS_IMAGE=i NAMESPACE=ns PREPULL_NODE_SELECTOR=k=v WEIGHTS_NODE_SELECTOR=w=x 2>/dev/null | tr -d '\\\n' || true)"
case "$line" in *'--node-selector "w=x"'*) ok "make weights: WEIGHTS_NODE_SELECTOR overrides PREPULL_NODE_SELECTOR" ;; *) fail "make weights override: $line" ;; esac
line="$(make -n weights WEIGHTS_MODEL=m WEIGHTS_PATH=/p WEIGHTS_IMAGE=i NAMESPACE=ns WEIGHTS_NODE_SELECTOR=k=v,w=x 2>/dev/null | tr -d '\\\n' || true)"
case "$line" in *'--node-selector "k=v,w=x"'*) ok "make weights: a two-pair WEIGHTS_NODE_SELECTOR reaches the script whole" ;; *) fail "make weights two pairs: $line" ;; esac
line="$(make -n weights-delete NAMESPACE=ns 2>/dev/null | grep 'weights.sh delete' || true)"
case "$line" in *"--all"*) ok "make weights-delete with no WEIGHTS_MODEL deletes every model's holder" ;; *) fail "weights-delete: $line" ;; esac
: > "$T/calls"
if PATH="$STUB_PATH" make weights-status > "$T/make-nons.out" 2>&1; then fail "make weights-status without NAMESPACE ran against the Makefile default"; else
    [ ! -s "$T/calls" ] && ok "make weights-status without NAMESPACE is refused before any kubectl call" || fail "make weights-status without NAMESPACE called kubectl: $(cat "$T/calls")"; fi
: > "$T/calls"
if PATH="$STUB_PATH" make weights WEIGHTS_PATH=/p WEIGHTS_IMAGE=i NAMESPACE=ns >/dev/null 2>&1; then fail "make weights without WEIGHTS_MODEL must be refused"; else
    [ ! -s "$T/calls" ] && ok "make weights without WEIGHTS_MODEL is refused before any kubectl call" || fail "make weights without WEIGHTS_MODEL called kubectl"; fi
line="$(make -n benchmark-standup BENCHMARK_NAMESPACE=ns BENCHMARK_MODEL_HOSTPATH=/mnt/local/w BENCHMARK_SPEC=guides/pd-disaggregation PREPULL_TOLERATIONS=t1 2>/dev/null | grep -A2 'model_hostpath.sh' || true)"
case "$line" in
    *'model_hostpath.sh'*'guides/pd-disaggregation.yaml'*'"/mnt/local/w" "ns" "" "t1"'*) ok "benchmark-standup: BENCHMARK_MODEL_HOSTPATH runs model_hostpath.sh on the scenario copy with the namespace, selector and tolerations" ;;
    *) fail "benchmark-standup model_hostpath step: $line" ;;
esac
n="$(make -n benchmark-standup BENCHMARK_NAMESPACE=ns BENCHMARK_MODEL_HOSTPATH=/mnt/local/w BENCHMARK_SPEC=guides/pd-disaggregation 2>/dev/null | grep -n 'model_hostpath.sh\|standup \\$' | head -2 | tr '\n' ' ')"
case "$n" in *model_hostpath*standup*) ok "benchmark-standup: the scenario edit precedes the harness standup" ;; *) fail "benchmark-standup ordering: $n" ;; esac

# ---------------------------------------------------------------------------
# 5. patch_harness.sh fix 11, run on a fixture carrying the upstream anchors:
#    the probe, the affinity, the best-effort chcon; idempotent; and the
#    earlier form (probe + affinity, chcon still fatal) completed.
# ---------------------------------------------------------------------------
sed -n '/fix 11 (download DaemonSet readiness + affinity) failed"/,/^PYEOF$/p' hack/benchmark/patch_harness.sh | sed '1d;$d' > "$T/fix11.py"
[ -s "$T/fix11.py" ] || fail "could not extract fix 11 from patch_harness.sh"
cat > "$T/03.j2" <<'EOF2'
      serviceAccountName: {{ serviceAccount.name }}
{% if storage.hostPath.tolerations is defined and storage.hostPath.tolerations %}
      tolerations:
{{ storage.hostPath.tolerations | toyaml | indent(8, true) }}
{% endif %}
      containers:
        - name: downloader
          image: {{ images.benchmark.repository }}:{{ images.benchmark.tag }}
          command: ["/bin/sh", "-c"]
          args:
            - |
              set -e
              hf download "{{ model.huggingfaceId }}" --local-dir "${TARGET_DIR}"
              echo "Relabelling for non-spc_t readers..."
              chcon -R -t container_file_t "{{ storage.hostPath.path }}"
              touch "${MARKER}"
EOF2
cp "$T/03.j2" "$T/03-orig.j2"
out="$("$PY" "$T/fix11.py" "$T/03.j2" 2>&1 || true)"
case "$out" in *"applied"*) ok "fix 11: applies to the upstream anchors" ;; *) fail "fix 11 on the fixture: $out" ;; esac
grep -q 'readinessProbe' "$T/03.j2" && grep -q '"test", "-f", "{{ storage.hostPath.path }}/{{ model.path }}/.download-complete"' "$T/03.j2" && ok "fix 11: the readiness probe tests the marker under the hostPath" || fail "fix 11: probe missing or wrong"
grep -q 'storage.hostPath.affinity is defined' "$T/03.j2" && ok "fix 11: an optional affinity renders" || fail "fix 11: affinity block missing"
out="$("$PY" "$T/fix11.py" "$T/03.j2" 2>&1 || true)"
case "$out" in *"already applied"*) ok "fix 11: idempotent" ;; *) fail "fix 11 second run: $out" ;; esac
# the earlier form: probe + affinity applied, chcon untouched -- completed, not refused
grep -q 'chcon -R -t container_file_t -l s0 "{{ storage.hostPath.path }}" 2>/tmp/chcon.err' "$T/03.j2" && grep -q 'grep -q "unlabeled file' "$T/03.j2" && grep -q 'cat /tmp/chcon.err; exit 1' "$T/03.j2" && ok "fix 11: chcon at level s0, skipped only on the no-SELinux text, fatal otherwise (a wrong SCC must not mark an unreadable directory)" || fail "fix 11: chcon form"
"$PY" - "$T/03.j2" <<'PYEOF' || fail "fix 11: could not rebuild the earlier forms"
import sys, re
src = open(sys.argv[1]).read()
a = src.index("              # scaling-manager-patch: skipped only where there is no SELinux")
b = src.index("\n              fi\n", src.index("cat /tmp/chcon.err; exit 1")) + len("\n              fi\n")
open(sys.argv[1].replace("03.j2", "03-earlier.j2"), "w").write(src[:a] + '              chcon -R -t container_file_t "{{ storage.hostPath.path }}"\n' + src[b:])
open(sys.argv[1].replace("03.j2", "03-prev.j2"), "w").write(src[:a] + '              chcon -R -t container_file_t "{{ storage.hostPath.path }}" 2>/dev/null || echo "Relabelling skipped: no SELinux on this node (scaling-manager-patch)."\n' + src[b:])
PYEOF
out="$("$PY" "$T/fix11.py" "$T/03-earlier.j2" 2>&1 || true)"
case "$out" in *"chcon made best-effort"*) ok "fix 11: the earliest form (fatal chcon) is completed in place" ;; *) fail "fix 11 on the earliest form: $out" ;; esac
out="$("$PY" "$T/fix11.py" "$T/03-prev.j2" 2>&1 || true)"
case "$out" in *"gated on the no-SELinux text"*) ok "fix 11: the previous form (chcon swallowed on any error) is completed in place" ;; *) fail "fix 11 on the previous form: $out" ;; esac
cmp -s "$T/03-earlier.j2" "$T/03.j2" && cmp -s "$T/03-prev.j2" "$T/03.j2" && ok "fix 11: both migrations end at the current form" || fail "fix 11: migrated templates differ from a fresh apply"
sed 's/images.benchmark.repository/images.other.repository/' "$T/03-orig.j2" > "$T/03-drift.j2"
if "$PY" "$T/fix11.py" "$T/03-drift.j2" >/dev/null 2>&1; then fail "fix 11 must fail on a missing anchor, not skip"; else ok "fix 11: a missing anchor is a hard error"; fi

# ---------------------------------------------------------------------------
# 6. patch_harness.sh fix 12: the harness volume named for its namespace,
#    with a claimRef; the claim's volumeName follows; the teardown scoped.
# ---------------------------------------------------------------------------
sed -n '/fix 12 (hostPath PV owned by its namespace) failed"/,/^PYEOF$/p' hack/benchmark/patch_harness.sh | sed '1d;$d' > "$T/fix12.py"
[ -s "$T/fix12.py" ] || fail "could not extract fix 12 from patch_harness.sh"
mkdir -p "$T/h12"
cat > "$T/h12/pv.j2" <<'EOF2'
metadata:
  name: {{ storage.modelPvc.name }}-hostpath-pv
  labels:
    app: {{ labels.app }}
    usage: model-cache
spec:
  capacity:
EOF2
cat > "$T/h12/pvc.j2" <<'EOF2'
  storageClassName: {{ storage.hostPath.storageClassName }}
  volumeName: {{ storage.modelPvc.name }}-hostpath-pv
EOF2
cat > "$T/h12/td.py" <<'EOF2'
class S:
    def a(self, cmd, context):
        pv_result = cmd.kube(
            "delete",
            "pv",
            "-l",
            "usage=model-cache",
            "--ignore-not-found",
        )
    def b(self, cmd, context):
        pv_result = cmd.kube(
            "delete",
            "pv",
            "-l",
            "usage=model-cache",
            "--ignore-not-found",
        )
EOF2
out="$("$PY" "$T/fix12.py" "$T/h12/pv.j2" "$T/h12/pvc.j2" "$T/h12/td.py" 2>&1 || true)"
case "$out" in *"applied"*) ok "fix 12: applies to the upstream anchors" ;; *) fail "fix 12 on the fixtures: $out" ;; esac
grep -q 'name: {{ storage.modelPvc.name }}-{{ namespace.name }}-hostpath-pv' "$T/h12/pv.j2" && grep -q 'volumeName: {{ storage.modelPvc.name }}-{{ namespace.name }}-hostpath-pv' "$T/h12/pvc.j2" && ok "fix 12: the volume is named for its namespace and the claim follows" || fail "fix 12: names"
grep -A2 'claimRef:' "$T/h12/pv.j2" | grep -q 'namespace: {{ namespace.name }}' && ok "fix 12: the volume carries a claimRef into its namespace" || fail "fix 12: claimRef missing"
grep -q 'scaling-manager.llmd.ai/model-namespace: {{ namespace.name }}' "$T/h12/pv.j2" && [ "$(grep -c 'usage=model-cache,scaling-manager.llmd.ai/model-namespace={context.require_namespace()}' "$T/h12/td.py")" -eq 2 ] && ok "fix 12: the teardown deletes only the volume labelled for the namespace being torn down (both sites)" || fail "fix 12: teardown scoping"
"$PY" -c "import ast,sys; ast.parse(open(sys.argv[1]).read())" "$T/h12/td.py" && ok "fix 12: the patched teardown still parses" || fail "fix 12: teardown does not parse"
out="$("$PY" "$T/fix12.py" "$T/h12/pv.j2" "$T/h12/pvc.j2" "$T/h12/td.py" 2>&1 || true)"
case "$out" in *"already applied"*) ok "fix 12: idempotent" ;; *) fail "fix 12 second run: $out" ;; esac
sed -i 's/usage: model-cache$/usage: cache/' "$T/h12/pv.j2"; sed -i 's/{{ namespace.name }}-hostpath-pv/hostpath-pv/' "$T/h12/pv.j2"
if "$PY" "$T/fix12.py" "$T/h12/pv.j2" "$T/h12/pvc.j2" "$T/h12/td.py" >/dev/null 2>&1; then fail "fix 12 must fail on a missing anchor"; else ok "fix 12: a missing anchor is a hard error"; fi

if [ "$FAILED" -ne 0 ]; then
    echo "weights checks: FAIL"
    exit 1
fi
echo "weights checks OK"
