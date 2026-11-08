"""staircase_promq.py <url> <namespace> <start epoch> <end epoch> : the staircase's Prometheus series.

query_range at 15 s, one JSON line a series. One failed series does not lose the others.
"""
import json
import sys
import urllib.parse
import urllib.request

url, ns, start, end = sys.argv[1:5]
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
    "fc_requests": 'sum by (outcome) (rate(llm_d_epp_flow_control_requests_total{namespace="%s"}[1m]))',
    "waiting": 'sum by (pod) (vllm:num_requests_waiting{namespace="%s"})',
    "running": 'sum by (pod) (vllm:num_requests_running{namespace="%s"})',
    "kv": 'max by (pod) (vllm:kv_cache_usage_perc{namespace="%s"})',
    "prompt_rate": 'sum by (pod) (rate(vllm:prompt_tokens_total{namespace="%s"}[1m]))',
    "ok_rate": 'sum by (pod) (rate(vllm:request_success_total{namespace="%s"}[1m]))',
    "engine_ttft_sum": 'sum by (pod) (rate(vllm:time_to_first_token_seconds_sum{namespace="%s"}[1m]))',
    "engine_ttft_count": 'sum by (pod) (rate(vllm:time_to_first_token_seconds_count{namespace="%s"}[1m]))',
}
for name, expr in Q.items():
    q = urllib.parse.urlencode({"query": expr % ns, "start": start, "end": end, "step": "15"})
    try:
        with urllib.request.urlopen(url + "/api/v1/query_range?" + q, timeout=120) as r:
            d = json.load(r)
        print(json.dumps({"name": name, "data": d.get("data"), "status": d.get("status")}))
    except Exception as e:  # one failed series must not lose the others
        print(json.dumps({"name": name, "error": str(e)[:300]}))
