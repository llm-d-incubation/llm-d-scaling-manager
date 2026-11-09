"""staircase_promq.py, run in the witness pod: the staircase's reads from inside the cluster.

  staircase_promq.py <url> <namespace> <start epoch> <end epoch>
      Prometheus: query_range at 15 s, one JSON line a series. One failed series does not
      lose the others.
  staircase_promq.py scrape <url> <token file>
      One /metrics page, read with the bearer token in the file; exits 1 with the status
      and the first 200 bytes of the answer when it is refused.

Why scrape is here: the EPP serves /metrics behind an authentication filter that wants a
bearer token allowed to GET /metrics. Through the API server's pod proxy no token arrives
(the API server strips the caller's credentials before it proxies), so the EPP answers
401 there; r1 read nothing from it. The controller and Prometheus present one, and so does
this, with the token the controller scrapes with.
"""
import json
import sys
import urllib.error
import urllib.parse
import urllib.request


def scrape(url, token_file):
    """The page at url, read with the token in token_file; exits 1 with what refused it."""
    with open(token_file, encoding="utf8") as f:
        token = f.read().strip()
    if not token:
        sys.exit("scrape: %s is empty" % token_file)
    req = urllib.request.Request(url, headers={"Authorization": "Bearer " + token})
    try:
        with urllib.request.urlopen(req, timeout=20) as r:
            sys.stdout.write(r.read().decode("utf8", "replace"))
    except urllib.error.HTTPError as e:
        sys.exit("scrape: %s answered %d: %r" % (url, e.code, e.read()[:200]))
    except (urllib.error.URLError, OSError) as e:
        sys.exit("scrape: %s: %s" % (url, str(e)[:200]))


Q = {
    "ttft_bucket": 'sum by (le) (rate(llm_d_epp_request_ttft_seconds_bucket{namespace="%s"}[1m]))',
    "ttft_count": 'sum(rate(llm_d_epp_request_ttft_seconds_count{namespace="%s"}[1m]))',
    "ttft_sum": 'sum(rate(llm_d_epp_request_ttft_seconds_sum{namespace="%s"}[1m]))',
    "tpot_bucket": 'sum by (le) (rate(llm_d_epp_request_streaming_tpot_seconds_bucket{namespace="%s"}[1m]))',
    "tpot_count": 'sum(rate(llm_d_epp_request_streaming_tpot_seconds_count{namespace="%s"}[1m]))',
    "tpot_sum": 'sum(rate(llm_d_epp_request_streaming_tpot_seconds_sum{namespace="%s"}[1m]))',
    "fc_saturation": 'max by (stage, inference_pool) (llm_d_epp_flow_control_pool_saturation{namespace="%s"})',
    "fc_queue": 'sum(llm_d_epp_flow_control_queue_size{namespace="%s"})',
    "fc_queue_bucket": 'sum by (le) (rate(llm_d_epp_flow_control_request_queue_duration_seconds_bucket{namespace="%s"}[1m]))',
    "fc_queue_sum": 'sum(rate(llm_d_epp_flow_control_request_queue_duration_seconds_sum{namespace="%s"}[1m]))',
    "fc_queue_count": 'sum(rate(llm_d_epp_flow_control_request_queue_duration_seconds_count{namespace="%s"}[1m]))',
    "fc_requests": 'sum by (outcome) (rate(llm_d_epp_flow_control_requests_total{namespace="%s"}[1m]))',
    "waiting": 'sum by (pod) (vllm:num_requests_waiting{namespace="%s"})',
    "running": 'sum by (pod) (vllm:num_requests_running{namespace="%s"})',
    "kv": 'max by (pod) (vllm:kv_cache_usage_perc{namespace="%s"})',
    "prompt_rate": 'sum by (pod) (rate(vllm:prompt_tokens_total{namespace="%s"}[1m]))',
    "ok_rate": 'sum by (pod) (rate(vllm:request_success_total{namespace="%s"}[1m]))',
    "engine_ttft_sum": 'sum by (pod) (rate(vllm:time_to_first_token_seconds_sum{namespace="%s"}[1m]))',
    "engine_ttft_count": 'sum by (pod) (rate(vllm:time_to_first_token_seconds_count{namespace="%s"}[1m]))',
}


def query(url, ns, start, end):
    """Every series of Q over the range, one JSON line each."""
    for name, expr in Q.items():
        q = urllib.parse.urlencode({"query": expr % ns, "start": start, "end": end, "step": "15"})
        try:
            with urllib.request.urlopen(url + "/api/v1/query_range?" + q, timeout=120) as r:
                d = json.load(r)
            print(json.dumps({"name": name, "data": d.get("data"), "status": d.get("status")}))
        except Exception as e:  # one failed series must not lose the others
            print(json.dumps({"name": name, "error": str(e)[:300]}))


if __name__ == "__main__":
    if len(sys.argv) == 4 and sys.argv[1] == "scrape":
        scrape(sys.argv[2], sys.argv[3])
    elif len(sys.argv) == 5:
        query(*sys.argv[1:5])
    else:
        sys.exit(__doc__)
