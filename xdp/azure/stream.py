# Posts session.log to the gateway while the test runs, one batch a second, so
# the live page can show it. Usage: python3 stream.py <log> <url> <run_id>
# The ingest key comes from LIVE_TEST_INGEST_KEY. Touch <log>.done to finish.
import json, os, sys, time, urllib.request

log, url, run_id = sys.argv[1], sys.argv[2].rstrip("/") + "/v1/live-test/ingest", sys.argv[3]
key = os.environ["LIVE_TEST_INGEST_KEY"]

def post(lines, status=""):
    body = json.dumps({"runId": run_id, "lines": lines, "status": status}).encode()
    req = urllib.request.Request(url, body, {"Content-Type": "application/json", "X-Ingest-Key": key})
    for _ in range(3):
        try:
            urllib.request.urlopen(req, timeout=10).read()
            return
        except Exception as e:  # the free Render instance may be waking up
            print("ingest:", e, file=sys.stderr)
            time.sleep(2)

sent = 0
while True:
    done = os.path.exists(log + ".done")
    text = open(log).read() if os.path.exists(log) else ""
    if not done:
        # Only whole lines. A line still being written (the attacker's status
        # codes arrive one at a time) would otherwise be sent half done and
        # never again, because only the line count is tracked.
        text = text[: text.rfind("\n") + 1]
    lines = text.splitlines()
    if len(lines) > sent:
        post(lines[sent:])
        sent = len(lines)
    if done:
        post([], open(log + ".done").read().strip() or "done")
        break
    time.sleep(1)
