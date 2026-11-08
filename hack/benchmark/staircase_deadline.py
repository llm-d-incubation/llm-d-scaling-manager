"""In-cluster deadline: delete the namespace's workloads when the runner's lease lapses, or at the
hard cap, whichever comes first. This is the backstop for a dead runner, so it must not be the
thing that waits.

The runner writes an epoch into ConfigMap staircase-lease (key until) every few minutes. If the
runner dies, the lease lapses and the GPUs are freed. It prints its armed line only after a read
of the lease has worked: staircase.sh refuses a run without that line, so a Job that cannot read
its lease (RBAC, a NetworkPolicy) is no backstop and holds nothing.

A read that fails keeps the last lease for a while, since an API blip is not the runner dying, but
not for ever: READ_BOUND of failed reads is a lapse. A pod that restarts has no last lease, so it
starts from the grace, never from the hard cap. Once lapsed, it retries the deletes for
DELETE_BOUND, and a kind the API does not have passes.

Runs as a ServiceAccount whose Role is scoped to its own namespace. Uses the Kubernetes API
directly, so the image only needs python3.
"""
import json
import os
import ssl
import sys
import time
import urllib.error
import urllib.request

SA = "/var/run/secrets/kubernetes.io/serviceaccount"
LEASE = "staircase-lease"
# Seconds: what a pod with no reading yet gives the lease, how long reads may fail before that is
# a lapse, how often it looks, how long it keeps trying to delete.
GRACE = 1800
READ_BOUND = 1800
EVERY = 60
DELETE_BOUND = 6 * 3600
PATHS = ("/apis/keda.sh/v1alpha1/namespaces/%s/scaledobjects",
         "/apis/leaderworkerset.x-k8s.io/v1/namespaces/%s/leaderworkersets",
         "/apis/apps/v1/namespaces/%s/deployments",
         "/apis/apps/v1/namespaces/%s/statefulsets",
         "/apis/apps/v1/namespaces/%s/daemonsets")


def stamp(t=None):
    return time.strftime("%FT%TZ", time.gmtime(t))


class Watch:
    """The lease as this process knows it."""

    def __init__(self, now, hard):
        self.hard = hard
        self.until = min(hard, now + GRACE)
        self.failing = None
        self.armed = False

    def look(self, now, read):
        """Take one read of the lease. True on the first that works."""
        try:
            self.until = read()
        except Exception as e:  # noqa: BLE001 -- bounded by lapsed
            self.failing = now if self.failing is None else self.failing
            print(stamp(), "lease read failed:", e, flush=True)
            return False
        self.failing = None
        first, self.armed = not self.armed, True
        return first

    def lapsed(self, now):
        """Whether the run's time is up: the lease, the hard cap, or reads failing past the bound."""
        return now >= min(self.until, self.hard) or (self.failing is not None and now - self.failing >= READ_BOUND)


def delete_all(ns, delete):
    """Delete every path; keep going past a failure, to free what can be freed. Returns 1 on any.

    A 404 is a kind this API does not have, which is nothing to free.
    """
    rc = 0
    for path in PATHS:
        try:
            print(stamp(), "DELETE", path % ns, delete(path % ns), flush=True)
        except urllib.error.HTTPError as e:
            if e.code != 404:
                print(stamp(), "DELETE", path % ns, "failed:", e, flush=True)
                rc = 1
        except Exception as e:  # noqa: BLE001 -- see above
            print(stamp(), "DELETE", path % ns, "failed:", e, flush=True)
            rc = 1
    return rc


def free(ns, delete, clock, sleep):
    """Delete until it works, or until DELETE_BOUND. Returns 0 when it did."""
    give_up = clock() + DELETE_BOUND
    while True:
        rc = delete_all(ns, delete)
        if rc == 0 or clock() >= give_up:
            return rc
        sleep(EVERY)


def main():
    hard = int(os.environ["DEADLINE_EPOCH"])
    with open(SA + "/namespace", encoding="utf8") as f:
        ns = f.read().strip()
    ctx = ssl.create_default_context(cafile=SA + "/ca.crt")
    host = "https://%s:%s" % (os.environ["KUBERNETES_SERVICE_HOST"], os.environ["KUBERNETES_SERVICE_PORT"])

    def token():
        with open(SA + "/token", encoding="utf8") as f:
            return f.read().strip()

    def read():
        req = urllib.request.Request(host + "/api/v1/namespaces/%s/configmaps/%s" % (ns, LEASE),
                                     headers={"Authorization": "Bearer " + token()})
        with urllib.request.urlopen(req, context=ctx, timeout=30) as r:
            return int(json.load(r)["data"]["until"])

    def delete(path):
        req = urllib.request.Request(host + path, method="DELETE", headers={
            "Authorization": "Bearer " + token(), "Content-Type": "application/json"},
            data=json.dumps({"kind": "DeleteOptions", "apiVersion": "v1", "propagationPolicy": "Background"}).encode())
        with urllib.request.urlopen(req, context=ctx, timeout=60) as r:
            return "%d items %d" % (r.status, len(json.load(r).get("items", [])))

    watch = Watch(time.time(), hard)
    last = None
    while True:
        if watch.look(time.time(), read):
            print(stamp(), "deadline at", stamp(hard), "or the lease, namespace", ns, flush=True)
        if watch.until != last:
            print(stamp(), "lease until", stamp(watch.until), flush=True)
            last = watch.until
        if watch.lapsed(time.time()):
            break
        time.sleep(EVERY)
    print(stamp(), "deadline reached: lease", watch.until, "hard", hard, "read failing since", watch.failing, flush=True)
    sys.exit(free(ns, delete, time.time, time.sleep))


if __name__ == "__main__":
    main()
