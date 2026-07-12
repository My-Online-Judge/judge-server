# Judge Kafka Microservice — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Chuyển việc chấm bài từ lời gọi HTTP đồng bộ trong judge-api sang giao tiếp event-driven qua Kafka, với một judge-worker Python độc lập, và đẩy verdict về client qua SSE.

**Architecture:** judge-api publish `submission.requested` rồi trả `PENDING` ngay. judge-worker (Python) consume, gọi qduoj judge-server, resolve verdict, publish `submission.judged`. judge-api consume kết quả, cập nhật DB (idempotent), đẩy SSE.

**Tech Stack:** Java 17 / Spring Boot 3.4.1 / spring-kafka (judge-api); Python 3.11 / kafka-python / requests (judge-worker); Kafka KRaft single broker; Postgres; qduoj judge-server (Docker).

## Global Constraints

- **Repos độc lập:** `judge-api`, `judge-worker` (mới), `judge-server` mỗi cái là git repo riêng. Commit của mỗi task nằm trong repo tương ứng. `my-oj` cha KHÔNG phải git repo.
- **Verdict codes phải khớp TUYỆT ĐỐI** enum `SubmissionResult` của judge-api: `COMPILE_ERROR=-2, WRONG_ANSWER=-1, ACCEPTED=0, CPU_TIME_LIMIT_EXCEEDED=1, REAL_TIME_LIMIT_EXCEEDED=2, MEMORY_LIMIT_EXCEEDED=3, RUNTIME_ERROR=4, SYSTEM_ERROR=5, PENDING=6, JUDGING=7, PARTIALLY_ACCEPTED=8`.
- **Topics:** `submission.requested` (judge-api→worker, group `judge-workers`), `submission.judged` (worker→judge-api, group `judge-api-results`). Message key = `submissionId` (string).
- **At-least-once:** worker commit offset *sau khi* publish kết quả. Idempotency: judge-api chỉ apply verdict nếu submission còn `PENDING` (=6).
- **judge-server endpoint:** `POST {JUDGE_SERVER_URL}/judge`, header `X-Judge-Server-Token: sha256_hex(token)`.
- **Event JSON dùng snake_case** cho các field judge (`language_config`, `max_cpu_time`, `max_memory`, `test_case_id`, `cpu_time`, `real_time`, `error_message`) và `submissionId` (camelCase) cho id — khớp DTO Java hiện có.

---

## Phase A — judge-worker (Python, service mới)

Toàn bộ Phase A làm trong repo mới `/home/tuan/Project/my-oj/judge-worker/`. Chạy test bằng `pytest` (không cần Kafka/judge-server thật — dùng fake/mock).

### Task 1: Scaffold worker + config + verdict constants

**Files:**
- Create: `judge-worker/requirements.txt`
- Create: `judge-worker/config.py`
- Create: `judge-worker/verdict.py`
- Create: `judge-worker/tests/__init__.py`
- Create: `judge-worker/tests/test_config.py`

**Interfaces:**
- Produces: `config.Config` (class với các thuộc tính env). `verdict` constants (`ACCEPTED=0`, `SYSTEM_ERROR=5`, `COMPILE_ERROR=-2`, …).

- [ ] **Step 1: Khởi tạo repo + venv**

```bash
mkdir -p /home/tuan/Project/my-oj/judge-worker/tests
cd /home/tuan/Project/my-oj/judge-worker
git init -q
python3 -m venv .venv && . .venv/bin/activate
printf "kafka-python==2.0.2\nrequests==2.32.3\npytest==8.3.3\n" > requirements.txt
pip install -q -r requirements.txt
printf ".venv/\n__pycache__/\n*.pyc\n" > .gitignore
touch tests/__init__.py
```

- [ ] **Step 2: Viết `verdict.py` (constants — chưa có logic resolve)**

```python
# judge-worker/verdict.py
"""Verdict codes — MUST match judge-api SubmissionResult enum exactly."""

COMPILE_ERROR = -2
WRONG_ANSWER = -1
ACCEPTED = 0
CPU_TIME_LIMIT_EXCEEDED = 1
REAL_TIME_LIMIT_EXCEEDED = 2
MEMORY_LIMIT_EXCEEDED = 3
RUNTIME_ERROR = 4
SYSTEM_ERROR = 5
```

- [ ] **Step 3: Viết failing test cho config**

```python
# judge-worker/tests/test_config.py
import importlib


def test_config_reads_env(monkeypatch):
    monkeypatch.setenv("KAFKA_BROKERS", "broker:9092")
    monkeypatch.setenv("JUDGE_SERVER_URL", "http://js:8080")
    monkeypatch.setenv("JUDGE_SERVER_TOKEN", "secret")
    import config
    importlib.reload(config)
    c = config.Config
    assert c.KAFKA_BROKERS == "broker:9092"
    assert c.JUDGE_SERVER_URL == "http://js:8080"
    assert c.JUDGE_SERVER_TOKEN == "secret"
    assert c.REQUESTED_TOPIC == "submission.requested"
    assert c.JUDGED_TOPIC == "submission.judged"
    assert c.CONSUMER_GROUP == "judge-workers"
```

- [ ] **Step 4: Chạy test — kỳ vọng FAIL**

Run: `cd /home/tuan/Project/my-oj/judge-worker && . .venv/bin/activate && python -m pytest tests/test_config.py -v`
Expected: FAIL — `ModuleNotFoundError: No module named 'config'`

- [ ] **Step 5: Viết `config.py`**

```python
# judge-worker/config.py
import os


class Config:
    KAFKA_BROKERS = os.getenv("KAFKA_BROKERS", "localhost:9092")
    JUDGE_SERVER_URL = os.getenv("JUDGE_SERVER_URL", "http://localhost:8080")
    JUDGE_SERVER_TOKEN = os.getenv("JUDGE_SERVER_TOKEN", "default_token")
    REQUESTED_TOPIC = os.getenv("REQUESTED_TOPIC", "submission.requested")
    JUDGED_TOPIC = os.getenv("JUDGED_TOPIC", "submission.judged")
    CONSUMER_GROUP = os.getenv("CONSUMER_GROUP", "judge-workers")
    JUDGE_TIMEOUT_SECONDS = int(os.getenv("JUDGE_TIMEOUT_SECONDS", "30"))
```

- [ ] **Step 6: Chạy test — kỳ vọng PASS**

Run: `python -m pytest tests/test_config.py -v`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add requirements.txt .gitignore config.py verdict.py tests/
git commit -m "chore: scaffold judge-worker with config and verdict constants"
```

---

### Task 2: `verdict.resolve_verdict` + `compute_stats` + `map_envelope_error`

**Files:**
- Modify: `judge-worker/verdict.py`
- Create: `judge-worker/tests/test_verdict.py`

**Interfaces:**
- Produces:
  - `resolve_verdict(details: list[dict]) -> int` — ACM mode: sandbox error (`error != 0`) → `SYSTEM_ERROR`; ngược lại testcase fail đầu tiên (`result != 0`) thắng; all-pass → `ACCEPTED`.
  - `compute_stats(details: list[dict]) -> tuple[int,int,int,int]` → `(max_cpu_time, max_real_time, max_memory, first_fail_result)`.
  - `map_envelope_error(err: str) -> int` → `COMPILE_ERROR` cho `CompileError`/`SPJCompileError`, ngược lại `SYSTEM_ERROR`.

- [ ] **Step 1: Viết failing tests (parity với logic Java `resolveVerdict`)**

```python
# judge-worker/tests/test_verdict.py
import verdict


def test_all_pass_is_accepted():
    details = [{"error": 0, "result": 0, "cpu_time": 5, "real_time": 6, "memory": 100},
               {"error": 0, "result": 0, "cpu_time": 3, "real_time": 4, "memory": 90}]
    assert verdict.resolve_verdict(details) == verdict.ACCEPTED


def test_first_failing_result_wins():
    details = [{"error": 0, "result": 0},
               {"error": 0, "result": verdict.WRONG_ANSWER},
               {"error": 0, "result": verdict.MEMORY_LIMIT_EXCEEDED}]
    assert verdict.resolve_verdict(details) == verdict.WRONG_ANSWER


def test_sandbox_error_overrides_everything():
    details = [{"error": 0, "result": verdict.WRONG_ANSWER},
               {"error": -1, "result": 0}]
    assert verdict.resolve_verdict(details) == verdict.SYSTEM_ERROR


def test_compute_stats_takes_maxima_and_first_fail():
    details = [{"error": 0, "result": 0, "cpu_time": 5, "real_time": 6, "memory": 100},
               {"error": 0, "result": 3, "cpu_time": 9, "real_time": 2, "memory": 400}]
    assert verdict.compute_stats(details) == (9, 6, 400, 3)


def test_compute_stats_handles_none_values():
    details = [{"cpu_time": None, "real_time": None, "memory": None, "result": 0}]
    assert verdict.compute_stats(details) == (0, 0, 0, 0)


def test_map_envelope_error():
    assert verdict.map_envelope_error("CompileError") == verdict.COMPILE_ERROR
    assert verdict.map_envelope_error("SPJCompileError") == verdict.COMPILE_ERROR
    assert verdict.map_envelope_error("TokenVerificationFailed") == verdict.SYSTEM_ERROR
```

- [ ] **Step 2: Chạy test — kỳ vọng FAIL**

Run: `python -m pytest tests/test_verdict.py -v`
Expected: FAIL — `AttributeError: module 'verdict' has no attribute 'resolve_verdict'`

- [ ] **Step 3: Bổ sung logic vào `verdict.py`**

```python
# thêm vào cuối judge-worker/verdict.py

def resolve_verdict(details):
    verdict_code = ACCEPTED
    for item in details:
        error = item.get("error")
        if error is not None and error != 0:
            return SYSTEM_ERROR
        result = item.get("result")
        if verdict_code == ACCEPTED and result is not None and result != 0:
            verdict_code = result
    return verdict_code


def compute_stats(details):
    if not details:
        return 0, 0, 0, 0
    max_cpu = max((d.get("cpu_time") or 0) for d in details)
    max_real = max((d.get("real_time") or 0) for d in details)
    max_mem = max((d.get("memory") or 0) for d in details)
    first_fail = 0
    for d in details:
        r = d.get("result")
        if r is not None and r != 0:
            first_fail = r
            break
    return max_cpu, max_real, max_mem, first_fail


def map_envelope_error(err):
    if err in ("CompileError", "SPJCompileError"):
        return COMPILE_ERROR
    return SYSTEM_ERROR
```

- [ ] **Step 4: Chạy test — kỳ vọng PASS**

Run: `python -m pytest tests/test_verdict.py -v`
Expected: PASS (6 passed)

- [ ] **Step 5: Commit**

```bash
git add verdict.py tests/test_verdict.py
git commit -m "feat: verdict resolution and stats parity with judge-api"
```

---

### Task 3: `dispatcher` — token + gọi judge-server

**Files:**
- Create: `judge-worker/dispatcher.py`
- Create: `judge-worker/tests/test_dispatcher.py`

**Interfaces:**
- Produces:
  - `token_header(token: str) -> str` = `sha256_hex(token)`.
  - `build_judge_body(event: dict) -> dict` — trích `src, language_config, max_cpu_time, max_memory, test_case_id, output` từ event.
  - `call_judge_server(base_url, token, body, timeout) -> dict` — POST `/judge`, trả JSON (`{"err":..., "data":...}`). Raise nếu HTTP lỗi.

- [ ] **Step 1: Viết failing tests (mock `requests.post`)**

```python
# judge-worker/tests/test_dispatcher.py
import hashlib
from unittest.mock import MagicMock, patch

import dispatcher


def test_token_header_is_sha256_hex():
    assert dispatcher.token_header("abc") == hashlib.sha256(b"abc").hexdigest()


def test_build_judge_body_picks_judge_fields():
    event = {
        "submissionId": "sub-1",
        "src": "code",
        "language_config": {"compile": {}, "run": {}},
        "max_cpu_time": 1000,
        "max_memory": 268435456,
        "test_case_id": "prob",
        "output": True,
    }
    body = dispatcher.build_judge_body(event)
    assert body == {
        "src": "code",
        "language_config": {"compile": {}, "run": {}},
        "max_cpu_time": 1000,
        "max_memory": 268435456,
        "test_case_id": "prob",
        "output": True,
    }
    assert "submissionId" not in body


@patch("dispatcher.requests.post")
def test_call_judge_server_posts_with_token_header(mock_post):
    resp = MagicMock()
    resp.json.return_value = {"err": None, "data": []}
    resp.raise_for_status.return_value = None
    mock_post.return_value = resp

    out = dispatcher.call_judge_server("http://js:8080", "secret", {"src": "x"}, timeout=30)

    assert out == {"err": None, "data": []}
    args, kwargs = mock_post.call_args
    assert args[0] == "http://js:8080/judge"
    assert kwargs["headers"]["X-Judge-Server-Token"] == hashlib.sha256(b"secret").hexdigest()
    assert kwargs["json"] == {"src": "x"}
    assert kwargs["timeout"] == 30
```

- [ ] **Step 2: Chạy test — kỳ vọng FAIL**

Run: `python -m pytest tests/test_dispatcher.py -v`
Expected: FAIL — `ModuleNotFoundError: No module named 'dispatcher'`

- [ ] **Step 3: Viết `dispatcher.py`**

```python
# judge-worker/dispatcher.py
import hashlib

import requests

_JUDGE_BODY_FIELDS = ("src", "language_config", "max_cpu_time", "max_memory", "test_case_id")


def token_header(token):
    return hashlib.sha256(token.encode("utf-8")).hexdigest()


def build_judge_body(event):
    body = {field: event[field] for field in _JUDGE_BODY_FIELDS}
    body["output"] = event.get("output", True)
    return body


def call_judge_server(base_url, token, body, timeout=30):
    resp = requests.post(
        f"{base_url}/judge",
        json=body,
        headers={
            "X-Judge-Server-Token": token_header(token),
            "Content-Type": "application/json",
        },
        timeout=timeout,
    )
    resp.raise_for_status()
    return resp.json()
```

- [ ] **Step 4: Chạy test — kỳ vọng PASS**

Run: `python -m pytest tests/test_dispatcher.py -v`
Expected: PASS (3 passed)

- [ ] **Step 5: Commit**

```bash
git add dispatcher.py tests/test_dispatcher.py
git commit -m "feat: dispatcher builds judge body and calls judge-server with sha256 token"
```

---

### Task 4: `handler.build_judged_event` + `process_event`

**Files:**
- Create: `judge-worker/handler.py`
- Create: `judge-worker/tests/test_handler.py`

**Interfaces:**
- Consumes: `dispatcher.build_judge_body`, `dispatcher.call_judge_server`, `verdict.*`, `config.Config`.
- Produces:
  - `build_judged_event(submission_id: str, judge_response: dict) -> dict` — map response judge-server → payload `submission.judged` (`submissionId, status, result, cpu_time, real_time, memory, error_message, details`).
  - `process_event(event: dict, config) -> dict` — gọi judge-server rồi build judged event.

- [ ] **Step 1: Viết failing tests**

```python
# judge-worker/tests/test_handler.py
from unittest.mock import patch

import handler
import verdict


def test_build_judged_event_success_array():
    resp = {"err": None, "data": [
        {"error": 0, "result": 0, "cpu_time": 5, "real_time": 6, "memory": 100},
        {"error": 0, "result": 0, "cpu_time": 9, "real_time": 2, "memory": 400},
    ]}
    out = handler.build_judged_event("sub-1", resp)
    assert out["submissionId"] == "sub-1"
    assert out["status"] == verdict.ACCEPTED
    assert out["result"] == 0
    assert out["cpu_time"] == 9
    assert out["real_time"] == 6
    assert out["memory"] == 400
    assert out["error_message"] is None
    assert len(out["details"]) == 2


def test_build_judged_event_compile_error_envelope():
    resp = {"err": "CompileError", "data": "line 3: syntax error"}
    out = handler.build_judged_event("sub-2", resp)
    assert out["status"] == verdict.COMPILE_ERROR
    assert out["error_message"] == "CompileError: line 3: syntax error"
    assert out["details"] == []


def test_build_judged_event_unexpected_data_is_system_error():
    resp = {"err": None, "data": None}
    out = handler.build_judged_event("sub-3", resp)
    assert out["status"] == verdict.SYSTEM_ERROR
    assert out["details"] == []


def test_process_event_calls_judge_server(monkeypatch):
    event = {"submissionId": "sub-9", "src": "x", "language_config": {},
             "max_cpu_time": 1000, "max_memory": 1, "test_case_id": "p", "output": True}

    class Cfg:
        JUDGE_SERVER_URL = "http://js:8080"
        JUDGE_SERVER_TOKEN = "t"
        JUDGE_TIMEOUT_SECONDS = 30

    with patch("handler.call_judge_server", return_value={"err": None, "data": []}) as m:
        out = handler.process_event(event, Cfg)
    m.assert_called_once()
    assert out["submissionId"] == "sub-9"
    assert out["status"] == verdict.ACCEPTED
```

- [ ] **Step 2: Chạy test — kỳ vọng FAIL**

Run: `python -m pytest tests/test_handler.py -v`
Expected: FAIL — `ModuleNotFoundError: No module named 'handler'`

- [ ] **Step 3: Viết `handler.py`**

```python
# judge-worker/handler.py
import verdict
from dispatcher import build_judge_body, call_judge_server


def build_judged_event(submission_id, judge_response):
    err = judge_response.get("err")
    data = judge_response.get("data")

    # Case 1: envelope-level error, e.g. {"err":"CompileError","data":"..."}.
    if err is not None:
        extra = "" if data is None else f": {data}"
        return _event(submission_id, verdict.map_envelope_error(err),
                      None, 0, 0, 0, f"{err}{extra}", [])

    # Case 2: per-testcase result array.
    if isinstance(data, list):
        status = verdict.resolve_verdict(data)
        max_cpu, max_real, max_mem, first_fail = verdict.compute_stats(data)
        return _event(submission_id, status, first_fail,
                      max_cpu, max_real, max_mem, None, data)

    # Case 3: anything else is a malformed response.
    return _event(submission_id, verdict.SYSTEM_ERROR,
                  None, 0, 0, 0, "Unexpected judge response", [])


def _event(submission_id, status, result, cpu, real, mem, error_message, details):
    return {
        "submissionId": submission_id,
        "status": status,
        "result": result,
        "cpu_time": cpu,
        "real_time": real,
        "memory": mem,
        "error_message": error_message,
        "details": details,
    }


def process_event(event, config):
    body = build_judge_body(event)
    judge_response = call_judge_server(
        config.JUDGE_SERVER_URL, config.JUDGE_SERVER_TOKEN, body,
        timeout=config.JUDGE_TIMEOUT_SECONDS,
    )
    return build_judged_event(event["submissionId"], judge_response)
```

- [ ] **Step 4: Chạy test — kỳ vọng PASS**

Run: `python -m pytest tests/test_handler.py -v`
Expected: PASS (4 passed)

- [ ] **Step 5: Commit**

```bash
git add handler.py tests/test_handler.py
git commit -m "feat: handler maps judge-server response into submission.judged event"
```

---

### Task 5: Consumer loop + `main.py` (commit-after-publish, error→SYSTEM_ERROR)

**Files:**
- Create: `judge-worker/worker.py`
- Create: `judge-worker/main.py`
- Create: `judge-worker/tests/test_worker.py`

**Interfaces:**
- Consumes: `handler.process_event`, `verdict.SYSTEM_ERROR`, `config.Config`.
- Produces:
  - `system_error_event(submission_id, message) -> dict`.
  - `handle_one(event, config, producer) -> None` — process (bắt exception → SYSTEM_ERROR event), `producer.send(JUDGED_TOPIC, key, value)`, `producer.flush()`.
  - `run(config, consumer, producer) -> None` — vòng lặp: mỗi message → `handle_one` → `consumer.commit()` (SAU khi publish).

- [ ] **Step 1: Viết failing tests (dùng fake consumer/producer, không cần Kafka)**

```python
# judge-worker/tests/test_worker.py
from unittest.mock import MagicMock, patch

import verdict
import worker


class Msg:
    def __init__(self, value):
        self.value = value


def test_system_error_event_shape():
    e = worker.system_error_event("s1", "boom")
    assert e["submissionId"] == "s1"
    assert e["status"] == verdict.SYSTEM_ERROR
    assert e["error_message"] == "boom"
    assert e["details"] == []


def test_handle_one_publishes_result():
    producer = MagicMock()
    cfg = MagicMock(JUDGED_TOPIC="submission.judged")
    event = {"submissionId": "s2"}
    with patch("worker.process_event", return_value={"submissionId": "s2", "status": 0}):
        worker.handle_one(event, cfg, producer)
    producer.send.assert_called_once_with("submission.judged", key="s2",
                                           value={"submissionId": "s2", "status": 0})
    producer.flush.assert_called_once()


def test_handle_one_on_judge_failure_publishes_system_error():
    producer = MagicMock()
    cfg = MagicMock(JUDGED_TOPIC="submission.judged")
    event = {"submissionId": "s3"}
    with patch("worker.process_event", side_effect=RuntimeError("judge down")):
        worker.handle_one(event, cfg, producer)
    sent = producer.send.call_args.kwargs["value"]
    assert sent["status"] == verdict.SYSTEM_ERROR
    assert "judge down" in sent["error_message"]


def test_run_commits_after_publish():
    producer = MagicMock()
    consumer = MagicMock()
    consumer.__iter__.return_value = [Msg({"submissionId": "s4"})]
    cfg = MagicMock(JUDGED_TOPIC="submission.judged")
    with patch("worker.process_event", return_value={"submissionId": "s4", "status": 0}):
        worker.run(cfg, consumer, producer)
    # publish trước, commit sau
    assert producer.send.called
    consumer.commit.assert_called_once()
```

- [ ] **Step 2: Chạy test — kỳ vọng FAIL**

Run: `python -m pytest tests/test_worker.py -v`
Expected: FAIL — `ModuleNotFoundError: No module named 'worker'`

- [ ] **Step 3: Viết `worker.py`**

```python
# judge-worker/worker.py
import logging

import verdict
from handler import process_event

log = logging.getLogger("judge-worker")


def system_error_event(submission_id, message):
    return {
        "submissionId": submission_id,
        "status": verdict.SYSTEM_ERROR,
        "result": None,
        "cpu_time": 0,
        "real_time": 0,
        "memory": 0,
        "error_message": message,
        "details": [],
    }


def handle_one(event, config, producer):
    submission_id = event.get("submissionId")
    try:
        judged = process_event(event, config)
    except Exception as exc:  # judge-server down / timeout / bad response
        log.exception("Judging failed for submission %s", submission_id)
        judged = system_error_event(submission_id, str(exc))
    producer.send(config.JUDGED_TOPIC, key=submission_id, value=judged)
    producer.flush()


def run(config, consumer, producer):
    for msg in consumer:
        handle_one(msg.value, config, producer)
        consumer.commit()  # AFTER publish → at-least-once
```

- [ ] **Step 4: Chạy test — kỳ vọng PASS**

Run: `python -m pytest tests/test_worker.py -v`
Expected: PASS (4 passed)

- [ ] **Step 5: Viết `main.py` (wiring Kafka thật — không có test tự động)**

```python
# judge-worker/main.py
import json
import logging

from kafka import KafkaConsumer, KafkaProducer

from config import Config
from worker import run

logging.basicConfig(level=logging.INFO)


def build_consumer(config):
    return KafkaConsumer(
        config.REQUESTED_TOPIC,
        bootstrap_servers=config.KAFKA_BROKERS.split(","),
        group_id=config.CONSUMER_GROUP,
        enable_auto_commit=False,
        auto_offset_reset="earliest",
        value_deserializer=lambda b: json.loads(b.decode("utf-8")),
    )


def build_producer(config):
    return KafkaProducer(
        bootstrap_servers=config.KAFKA_BROKERS.split(","),
        key_serializer=lambda k: k.encode("utf-8"),
        value_serializer=lambda v: json.dumps(v).encode("utf-8"),
    )


if __name__ == "__main__":
    cfg = Config
    logging.info("judge-worker starting, brokers=%s", cfg.KAFKA_BROKERS)
    run(cfg, build_consumer(cfg), build_producer(cfg))
```

- [ ] **Step 6: Chạy toàn bộ test worker — kỳ vọng PASS**

Run: `python -m pytest -v`
Expected: PASS (tất cả file: config, verdict, dispatcher, handler, worker)

- [ ] **Step 7: Commit**

```bash
git add worker.py main.py tests/test_worker.py
git commit -m "feat: worker loop with commit-after-publish and Kafka wiring"
```

---

### Task 6: Dockerfile + README worker

**Files:**
- Create: `judge-worker/Dockerfile`
- Create: `judge-worker/README.md`

**Interfaces:** không có interface code mới.

- [ ] **Step 1: Viết `Dockerfile`**

```dockerfile
# judge-worker/Dockerfile
FROM python:3.11-slim
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir kafka-python==2.0.2 requests==2.32.3
COPY *.py ./
CMD ["python", "main.py"]
```

- [ ] **Step 2: Viết `README.md`**

```markdown
# judge-worker

Kafka consumer bọc quanh qduoj judge-server. Consume `submission.requested`,
gọi judge-server `/judge`, resolve verdict, publish `submission.judged`.

## Run tests
    python -m venv .venv && . .venv/bin/activate
    pip install -r requirements.txt
    python -m pytest -v

## Env
- KAFKA_BROKERS (default localhost:9092)
- JUDGE_SERVER_URL (default http://localhost:8080)
- JUDGE_SERVER_TOKEN (default default_token)
- REQUESTED_TOPIC / JUDGED_TOPIC / CONSUMER_GROUP / JUDGE_TIMEOUT_SECONDS
```

- [ ] **Step 3: Build thử image — kỳ vọng thành công**

Run: `cd /home/tuan/Project/my-oj/judge-worker && docker build -t judge-worker:dev .`
Expected: build hoàn tất, `Successfully tagged judge-worker:dev`

- [ ] **Step 4: Commit**

```bash
git add Dockerfile README.md
git commit -m "chore: dockerfile and readme for judge-worker"
```

---

## Phase B — judge-api (Spring Boot)

Toàn bộ Phase B làm trong repo `judge-api`. Đường dẫn gốc source: `judge-api/src/main/java/vn/thanhtuanle/`. Test: `cd judge-api && ./mvnw test` (hoặc `mvn test`).

### Task 7: spring-kafka + config + event DTOs

**Files:**
- Modify: `judge-api/pom.xml`
- Modify: `judge-api/src/main/resources/application.yml`
- Create: `.../messaging/KafkaTopics.java`
- Create: `.../messaging/event/SubmissionRequestedEvent.java`
- Create: `.../messaging/event/SubmissionJudgedEvent.java`

**Interfaces:**
- Produces:
  - `KafkaTopics.SUBMISSION_REQUESTED = "submission.requested"`, `KafkaTopics.SUBMISSION_JUDGED = "submission.judged"`.
  - `SubmissionRequestedEvent{ String submissionId; String src; JudgeLanguageConfigDto languageConfig; Integer maxCpuTime; Long maxMemory; String testCaseId; Boolean output; }`.
  - `SubmissionJudgedEvent{ String submissionId; Integer status; Integer result; Integer cpuTime; Integer realTime; Long memory; String errorMessage; List<JudgeResultDto> details; }`.

- [ ] **Step 1: Thêm dependency spring-kafka vào `pom.xml`**

Thêm vào khối `<dependencies>`:

```xml
<dependency>
    <groupId>org.springframework.kafka</groupId>
    <artifactId>spring-kafka</artifactId>
</dependency>
<dependency>
    <groupId>org.springframework.kafka</groupId>
    <artifactId>spring-kafka-test</artifactId>
    <scope>test</scope>
</dependency>
```

- [ ] **Step 2: Cấu hình Kafka trong `application.yml`**

Thêm vào cuối file (dưới khối `judge:`):

```yaml
spring:
  kafka:
    bootstrap-servers: ${KAFKA_BROKERS:localhost:9092}
    producer:
      key-serializer: org.apache.kafka.common.serialization.StringSerializer
      value-serializer: org.springframework.kafka.support.serializer.JsonSerializer
    consumer:
      group-id: judge-api-results
      auto-offset-reset: earliest
      key-deserializer: org.apache.kafka.common.serialization.StringDeserializer
      value-deserializer: org.springframework.kafka.support.serializer.JsonDeserializer
      properties:
        spring.json.trusted.packages: "vn.thanhtuanle.messaging.event"
        spring.json.value.default.type: "vn.thanhtuanle.messaging.event.SubmissionJudgedEvent"
```

> Lưu ý: `application.yml` đã có một khối `spring:` ở đầu file. GỘP `kafka:` vào đúng khối `spring:` hiện có thay vì tạo khối `spring:` thứ hai (YAML không cho phép trùng key gốc). Đặt `kafka:` ngang cấp với `application:` và `profiles:`.

- [ ] **Step 3: Viết `KafkaTopics.java`**

```java
// judge-api/src/main/java/vn/thanhtuanle/messaging/KafkaTopics.java
package vn.thanhtuanle.messaging;

public final class KafkaTopics {
    private KafkaTopics() {}

    public static final String SUBMISSION_REQUESTED = "submission.requested";
    public static final String SUBMISSION_JUDGED = "submission.judged";
}
```

- [ ] **Step 4: Viết `SubmissionRequestedEvent.java`**

```java
// judge-api/src/main/java/vn/thanhtuanle/messaging/event/SubmissionRequestedEvent.java
package vn.thanhtuanle.messaging.event;

import com.fasterxml.jackson.annotation.JsonProperty;
import lombok.AllArgsConstructor;
import lombok.Builder;
import lombok.Data;
import lombok.NoArgsConstructor;
import vn.thanhtuanle.judge.dto.JudgeLanguageConfigDto;

@Data
@Builder
@NoArgsConstructor
@AllArgsConstructor
public class SubmissionRequestedEvent {
    private String submissionId;
    private String src;

    @JsonProperty("language_config")
    private JudgeLanguageConfigDto languageConfig;

    @JsonProperty("max_cpu_time")
    private Integer maxCpuTime;

    @JsonProperty("max_memory")
    private Long maxMemory;

    @JsonProperty("test_case_id")
    private String testCaseId;

    private Boolean output;
}
```

- [ ] **Step 5: Viết `SubmissionJudgedEvent.java`**

```java
// judge-api/src/main/java/vn/thanhtuanle/messaging/event/SubmissionJudgedEvent.java
package vn.thanhtuanle.messaging.event;

import com.fasterxml.jackson.annotation.JsonProperty;
import lombok.AllArgsConstructor;
import lombok.Builder;
import lombok.Data;
import lombok.NoArgsConstructor;
import vn.thanhtuanle.judge.dto.JudgeResultDto;

import java.util.List;

@Data
@Builder
@NoArgsConstructor
@AllArgsConstructor
public class SubmissionJudgedEvent {
    private String submissionId;
    private Integer status;
    private Integer result;

    @JsonProperty("cpu_time")
    private Integer cpuTime;

    @JsonProperty("real_time")
    private Integer realTime;

    private Long memory;

    @JsonProperty("error_message")
    private String errorMessage;

    private List<JudgeResultDto> details;
}
```

- [ ] **Step 6: Biên dịch — kỳ vọng thành công**

Run: `cd judge-api && ./mvnw -q compile`
Expected: BUILD SUCCESS

- [ ] **Step 7: Commit**

```bash
cd judge-api
git add pom.xml src/main/resources/application.yml src/main/java/vn/thanhtuanle/messaging/
git commit -m "feat: add spring-kafka, topics and submission event DTOs"
```

---

### Task 8: Producer + refactor `submit()` trả PENDING (bỏ HTTP đồng bộ)

**Files:**
- Create: `.../messaging/SubmissionEventPublisher.java`
- Modify: `.../judge/JudgeService.java`
- Modify: `.../submission/SubmissionService.java`
- Create: `.../submission/SubmissionServiceSubmitTest.java` (test)

**Interfaces:**
- Consumes: `KafkaTopics`, `SubmissionRequestedEvent`, `JudgeService.buildRequestedEvent(...)`.
- Produces:
  - `SubmissionEventPublisher.publishRequested(SubmissionRequestedEvent event)` — gửi lên `SUBMISSION_REQUESTED`, key = `submissionId`.
  - `JudgeService.buildRequestedEvent(String submissionId, String sourceCode, Problem problem, Language language) -> SubmissionRequestedEvent` (trích từ `judge()` cũ, KHÔNG gọi HTTP).

- [ ] **Step 1: Viết `SubmissionEventPublisher.java`**

```java
// judge-api/src/main/java/vn/thanhtuanle/messaging/SubmissionEventPublisher.java
package vn.thanhtuanle.messaging;

import lombok.RequiredArgsConstructor;
import lombok.extern.slf4j.Slf4j;
import org.springframework.kafka.core.KafkaTemplate;
import org.springframework.stereotype.Component;
import vn.thanhtuanle.messaging.event.SubmissionRequestedEvent;

@Component
@RequiredArgsConstructor
@Slf4j
public class SubmissionEventPublisher {

    private final KafkaTemplate<String, Object> kafkaTemplate;

    public void publishRequested(SubmissionRequestedEvent event) {
        log.info("Publishing submission.requested for {}", event.getSubmissionId());
        kafkaTemplate.send(KafkaTopics.SUBMISSION_REQUESTED, event.getSubmissionId(), event);
    }
}
```

- [ ] **Step 2: Refactor `JudgeService.java` — thêm `buildRequestedEvent`, bỏ HTTP**

Thay method `judge(...)` bằng `buildRequestedEvent(...)`; XÓA các field `RestClient.Builder restClientBuilder`, `@Value judgeServerUrl`, `@Value judgeServerToken` và import liên quan (`RestClient`, `MediaType`, `HashUtil`). GIỮ nguyên `validate(...)`, `MEMORY_CHECK_ONLY_LANGUAGES`, `BLACKLISTED_KEYWORDS`, các hằng `MAX_CODE_LENGTH`, `BYTES_PER_MB`.

```java
// thay thân class JudgeService: giữ validate(); thay judge() bằng buildRequestedEvent()
    public SubmissionRequestedEvent buildRequestedEvent(String submissionId, String sourceCode,
            Problem problem, Language language) {
        log.info("Building judge request for problem: {}", problem.getProblemSlug());

        JudgeCompileConfigDto compileConfig = JudgeCompileConfigDto.builder()
                .srcName(language.getSrcName())
                .exeName(language.getExeName())
                .maxCpuTime(3000)
                .maxRealTime(5000)
                .maxMemory(language.getCompileMaxMemory())
                .compileCommand(language.getCompileCommand())
                .build();

        int memoryCheckOnly = MEMORY_CHECK_ONLY_LANGUAGES.contains(language.getIdentifier()) ? 1 : 0;
        JudgeRunConfigDto runConfig = JudgeRunConfigDto.builder()
                .command(language.getRunCommand())
                .seccompRule(language.getSeccompRule())
                .env(AppProperties.JUDGE_ENV)
                .memoryLimitCheckOnly(memoryCheckOnly)
                .build();

        JudgeLanguageConfigDto languageConfig = JudgeLanguageConfigDto.builder()
                .compile(compileConfig)
                .run(runConfig)
                .build();

        return SubmissionRequestedEvent.builder()
                .submissionId(submissionId)
                .src(sourceCode)
                .languageConfig(languageConfig)
                .maxCpuTime(problem.getTimeLimit())
                .maxMemory(problem.getMemoryLimit() * BYTES_PER_MB)
                .testCaseId(problem.getProblemSlug())
                .output(true)
                .build();
    }
```

Thêm import: `import vn.thanhtuanle.messaging.event.SubmissionRequestedEvent;`

- [ ] **Step 3: Viết failing test cho `submit()`**

```java
// judge-api/src/test/java/vn/thanhtuanle/submission/SubmissionServiceSubmitTest.java
package vn.thanhtuanle.submission;

import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.extension.ExtendWith;
import org.mockito.ArgumentCaptor;
import org.mockito.InjectMocks;
import org.mockito.Mock;
import org.mockito.junit.jupiter.MockitoExtension;

import com.fasterxml.jackson.databind.ObjectMapper;

import vn.thanhtuanle.common.enums.SubmissionResult;
import vn.thanhtuanle.entity.Language;
import vn.thanhtuanle.entity.Problem;
import vn.thanhtuanle.entity.Submission;
import vn.thanhtuanle.entity.User;
import vn.thanhtuanle.judge.JudgeService;
import vn.thanhtuanle.language.LanguageRepository;
import vn.thanhtuanle.messaging.SubmissionEventPublisher;
import vn.thanhtuanle.messaging.event.SubmissionRequestedEvent;
import vn.thanhtuanle.problem.ProblemRepository;
import vn.thanhtuanle.submission.dto.SubmissionRequestDto;
import vn.thanhtuanle.submission.dto.SubmissionResponseDto;
import vn.thanhtuanle.submission.mapper.SubmissionMapper;
import vn.thanhtuanle.user.UserService;

import java.util.Optional;

import static org.assertj.core.api.Assertions.assertThat;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.Mockito.*;

@ExtendWith(MockitoExtension.class)
class SubmissionServiceSubmitTest {

    @Mock SubmissionRepository submissionRepository;
    @Mock JudgeService judgeService;
    @Mock ProblemRepository problemRepository;
    @Mock LanguageRepository languageRepository;
    @Mock ObjectMapper objectMapper;
    @Mock SubmissionMapper submissionMapper;
    @Mock UserService userService;
    @Mock SubmissionEventPublisher eventPublisher;

    @InjectMocks SubmissionService submissionService;

    @Test
    void submit_savesPending_publishesEvent_andReturnsPending() {
        SubmissionRequestDto req = SubmissionRequestDto.builder()
                .sourceCode("int main(){}").languageIdentifier("cpp")
                .problemSlug("a-plus-b").shareSubmission(false).build();

        Problem problem = new Problem();
        Language language = new Language();
        User user = new User();

        when(problemRepository.findByProblemSlug("a-plus-b")).thenReturn(Optional.of(problem));
        when(languageRepository.findByIdentifier("cpp")).thenReturn(Optional.of(language));
        when(userService.getCurrentUser()).thenReturn(user);
        when(judgeService.buildRequestedEvent(any(), any(), any(), any()))
                .thenReturn(SubmissionRequestedEvent.builder().submissionId("x").build());
        when(submissionMapper.toDto(any(Submission.class)))
                .thenReturn(SubmissionResponseDto.builder()
                        .status(SubmissionResult.PENDING.getValue()).build());

        SubmissionResponseDto dto = submissionService.submit(req);

        verify(judgeService).validate("int main(){}");
        ArgumentCaptor<Submission> saved = ArgumentCaptor.forClass(Submission.class);
        verify(submissionRepository).save(saved.capture());
        assertThat(saved.getValue().getStatus()).isEqualTo(SubmissionResult.PENDING.getValue());
        verify(eventPublisher).publishRequested(any(SubmissionRequestedEvent.class));
        assertThat(dto.getStatus()).isEqualTo(SubmissionResult.PENDING.getValue());
    }
}
```

- [ ] **Step 4: Chạy test — kỳ vọng FAIL (compile error: submit chưa dùng publisher/buildRequestedEvent)**

Run: `cd judge-api && ./mvnw -q test -Dtest=SubmissionServiceSubmitTest`
Expected: FAIL (compile error hoặc assertion — `SubmissionService` chưa có field `eventPublisher`)

- [ ] **Step 5: Refactor `SubmissionService.java`**

Trong `SubmissionService`:
- Thêm field `private final SubmissionEventPublisher eventPublisher;` + import `SubmissionEventPublisher`, `SubmissionRequestedEvent`.
- Thay thân `submit(...)` (từ sau khi save PENDING) và XÓA các method `handleJudgeResponse`, `processJudgeResponse`, `mapEnvelopeError`, `resolveVerdict` (đã chuyển sang worker). Cũng bỏ field `ObjectMapper objectMapper` nếu không còn dùng nơi khác trong file.

```java
    @Transactional
    public SubmissionResponseDto submit(SubmissionRequestDto req) {
        log.info("Start submission for problem: {}", req.getProblemSlug());
        judgeService.validate(req.getSourceCode());

        Problem problem = problemRepository.findByProblemSlug(req.getProblemSlug())
                .orElseThrow(() -> new ResourceNotFoundException("Problem not found"));
        Language language = languageRepository.findByIdentifier(req.getLanguageIdentifier())
                .orElseThrow(() -> new ResourceNotFoundException("Language not found"));
        User currentUser = userService.getCurrentUser();

        Submission submission = createPendingSubmission(req, problem, language, currentUser);
        submissionRepository.save(submission);

        SubmissionRequestedEvent event = judgeService.buildRequestedEvent(
                submission.getId().toString(), submission.getSourceCode(), problem, language);
        eventPublisher.publishRequested(event);

        log.info("Submission {} queued for judging", submission.getId());
        return submissionMapper.toDto(submission);
    }
```

> `createPendingSubmission` giữ nguyên (đã set status = `PENDING`). `submission.getId()` có sẵn sau `save()` (UUID từ `BaseEntity`).

- [ ] **Step 6: Chạy test — kỳ vọng PASS**

Run: `cd judge-api && ./mvnw -q test -Dtest=SubmissionServiceSubmitTest`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
cd judge-api
git add src/main/java/vn/thanhtuanle/messaging/SubmissionEventPublisher.java \
        src/main/java/vn/thanhtuanle/judge/JudgeService.java \
        src/main/java/vn/thanhtuanle/submission/SubmissionService.java \
        src/test/java/vn/thanhtuanle/submission/SubmissionServiceSubmitTest.java
git commit -m "feat: submit publishes submission.requested and returns PENDING"
```

---

### Task 9: `JudgeResultConsumer` với idempotency

**Files:**
- Create: `.../messaging/JudgeResultConsumer.java`
- Create: `.../messaging/JudgeResultConsumerTest.java` (test)

**Interfaces:**
- Consumes: `SubmissionJudgedEvent`, `SubmissionRepository`, `KafkaTopics.SUBMISSION_JUDGED`, `SubmissionResult`.
- Produces: `JudgeResultConsumer.onJudged(SubmissionJudgedEvent event)` — load submission, chỉ apply nếu `status == PENDING`, set verdict/stats + save.

- [ ] **Step 1: Viết failing test**

```java
// judge-api/src/test/java/vn/thanhtuanle/messaging/JudgeResultConsumerTest.java
package vn.thanhtuanle.messaging;

import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.extension.ExtendWith;
import org.mockito.InjectMocks;
import org.mockito.Mock;
import org.mockito.junit.jupiter.MockitoExtension;

import vn.thanhtuanle.common.enums.SubmissionResult;
import vn.thanhtuanle.entity.Submission;
import vn.thanhtuanle.messaging.event.SubmissionJudgedEvent;
import vn.thanhtuanle.submission.SubmissionRepository;

import java.util.Optional;
import java.util.UUID;

import static org.assertj.core.api.Assertions.assertThat;
import static org.mockito.Mockito.*;

@ExtendWith(MockitoExtension.class)
class JudgeResultConsumerTest {

    @Mock SubmissionRepository submissionRepository;
    @InjectMocks JudgeResultConsumer consumer;

    private Submission pending(UUID id) {
        Submission s = Submission.builder().status(SubmissionResult.PENDING.getValue()).build();
        s.setId(id);
        return s;
    }

    @Test
    void appliesVerdict_whenPending() {
        UUID id = UUID.randomUUID();
        Submission s = pending(id);
        when(submissionRepository.findById(id)).thenReturn(Optional.of(s));

        SubmissionJudgedEvent e = SubmissionJudgedEvent.builder()
                .submissionId(id.toString()).status(SubmissionResult.ACCEPTED.getValue())
                .result(0).cpuTime(12).realTime(15).memory(3072L).build();

        consumer.onJudged(e);

        assertThat(s.getStatus()).isEqualTo(SubmissionResult.ACCEPTED.getValue());
        assertThat(s.getCpuTime()).isEqualTo(12);
        assertThat(s.getTime()).isEqualTo(15);
        assertThat(s.getMemory()).isEqualTo(3072L);
        verify(submissionRepository).save(s);
    }

    @Test
    void ignoresResult_whenAlreadyFinished() {
        UUID id = UUID.randomUUID();
        Submission s = Submission.builder().status(SubmissionResult.ACCEPTED.getValue()).build();
        s.setId(id);
        when(submissionRepository.findById(id)).thenReturn(Optional.of(s));

        SubmissionJudgedEvent e = SubmissionJudgedEvent.builder()
                .submissionId(id.toString()).status(SubmissionResult.WRONG_ANSWER.getValue()).build();

        consumer.onJudged(e);

        assertThat(s.getStatus()).isEqualTo(SubmissionResult.ACCEPTED.getValue());
        verify(submissionRepository, never()).save(any());
    }
}
```

- [ ] **Step 2: Chạy test — kỳ vọng FAIL**

Run: `cd judge-api && ./mvnw -q test -Dtest=JudgeResultConsumerTest`
Expected: FAIL — không tồn tại class `JudgeResultConsumer`

- [ ] **Step 3: Viết `JudgeResultConsumer.java`**

```java
// judge-api/src/main/java/vn/thanhtuanle/messaging/JudgeResultConsumer.java
package vn.thanhtuanle.messaging;

import lombok.RequiredArgsConstructor;
import lombok.extern.slf4j.Slf4j;
import org.springframework.kafka.annotation.KafkaListener;
import org.springframework.stereotype.Component;
import org.springframework.transaction.annotation.Transactional;
import vn.thanhtuanle.common.enums.SubmissionResult;
import vn.thanhtuanle.entity.Submission;
import vn.thanhtuanle.messaging.event.SubmissionJudgedEvent;
import vn.thanhtuanle.submission.SubmissionRepository;

import java.util.UUID;

@Component
@RequiredArgsConstructor
@Slf4j
public class JudgeResultConsumer {

    private final SubmissionRepository submissionRepository;

    @KafkaListener(topics = KafkaTopics.SUBMISSION_JUDGED, groupId = "judge-api-results")
    @Transactional
    public void onJudged(SubmissionJudgedEvent event) {
        UUID id = UUID.fromString(event.getSubmissionId());
        Submission submission = submissionRepository.findById(id).orElse(null);
        if (submission == null) {
            log.warn("Received verdict for unknown submission {}", id);
            return;
        }
        // Idempotency: only the first terminal verdict wins; duplicates are ignored.
        if (submission.getStatus() != SubmissionResult.PENDING.getValue()) {
            log.info("Submission {} already finished (status={}), ignoring verdict",
                    id, submission.getStatus());
            return;
        }
        submission.setStatus(event.getStatus());
        submission.setResult(event.getResult());
        submission.setCpuTime(event.getCpuTime());
        submission.setTime(event.getRealTime());
        submission.setMemory(event.getMemory());
        submission.setErrorMessage(event.getErrorMessage());
        submissionRepository.save(submission);
        log.info("Applied verdict {} to submission {}", event.getStatus(), id);
    }
}
```

- [ ] **Step 4: Chạy test — kỳ vọng PASS**

Run: `cd judge-api && ./mvnw -q test -Dtest=JudgeResultConsumerTest`
Expected: PASS (2 tests)

- [ ] **Step 5: Commit**

```bash
cd judge-api
git add src/main/java/vn/thanhtuanle/messaging/JudgeResultConsumer.java \
        src/test/java/vn/thanhtuanle/messaging/JudgeResultConsumerTest.java
git commit -m "feat: consume submission.judged and apply verdict idempotently"
```

---

### Task 10: SSE endpoint + emitter registry + wire vào consumer

**Files:**
- Create: `.../submission/SubmissionSseRegistry.java`
- Create: `.../submission/SubmissionSseRegistryTest.java` (test)
- Modify: `.../submission/SubmissionController.java`
- Modify: `.../messaging/JudgeResultConsumer.java`

**Interfaces:**
- Produces:
  - `SubmissionSseRegistry.subscribe(String submissionId) -> SseEmitter`.
  - `SubmissionSseRegistry.complete(String submissionId, SubmissionResponseDto payload)` — emit + complete + remove; no-op nếu không có subscriber.
- Consumes (JudgeResultConsumer): `SubmissionSseRegistry`, `SubmissionMapper` (tạo `SubmissionResponseDto` từ submission đã cập nhật).

- [ ] **Step 1: Viết failing test cho registry**

```java
// judge-api/src/test/java/vn/thanhtuanle/submission/SubmissionSseRegistryTest.java
package vn.thanhtuanle.submission;

import org.junit.jupiter.api.Test;
import org.springframework.web.servlet.mvc.method.annotation.SseEmitter;
import vn.thanhtuanle.submission.dto.SubmissionResponseDto;

import static org.assertj.core.api.Assertions.assertThat;
import static org.assertj.core.api.Assertions.assertThatCode;

class SubmissionSseRegistryTest {

    @Test
    void subscribeReturnsEmitter_andCompleteRemovesIt() {
        SubmissionSseRegistry registry = new SubmissionSseRegistry();
        SseEmitter emitter = registry.subscribe("sub-1");
        assertThat(emitter).isNotNull();

        // complete với subscriber tồn tại: không ném lỗi
        assertThatCode(() -> registry.complete("sub-1",
                SubmissionResponseDto.builder().status(0).build())).doesNotThrowAnyException();

        // complete lần 2 (đã remove) là no-op
        assertThatCode(() -> registry.complete("sub-1",
                SubmissionResponseDto.builder().status(0).build())).doesNotThrowAnyException();
    }

    @Test
    void completeUnknownSubmissionIsNoOp() {
        SubmissionSseRegistry registry = new SubmissionSseRegistry();
        assertThatCode(() -> registry.complete("nope",
                SubmissionResponseDto.builder().status(0).build())).doesNotThrowAnyException();
    }
}
```

- [ ] **Step 2: Chạy test — kỳ vọng FAIL**

Run: `cd judge-api && ./mvnw -q test -Dtest=SubmissionSseRegistryTest`
Expected: FAIL — không tồn tại class `SubmissionSseRegistry`

- [ ] **Step 3: Viết `SubmissionSseRegistry.java`**

```java
// judge-api/src/main/java/vn/thanhtuanle/submission/SubmissionSseRegistry.java
package vn.thanhtuanle.submission;

import lombok.extern.slf4j.Slf4j;
import org.springframework.stereotype.Component;
import org.springframework.web.servlet.mvc.method.annotation.SseEmitter;
import vn.thanhtuanle.submission.dto.SubmissionResponseDto;

import java.io.IOException;
import java.util.Map;
import java.util.concurrent.ConcurrentHashMap;

/**
 * In-memory registry of SSE subscribers keyed by submissionId.
 * NOTE: single-instance only; multi-instance fan-out would need a shared bus (out of scope).
 */
@Component
@Slf4j
public class SubmissionSseRegistry {

    private static final long TIMEOUT_MS = 5 * 60 * 1000L;
    private final Map<String, SseEmitter> emitters = new ConcurrentHashMap<>();

    public SseEmitter subscribe(String submissionId) {
        SseEmitter emitter = new SseEmitter(TIMEOUT_MS);
        emitters.put(submissionId, emitter);
        emitter.onCompletion(() -> emitters.remove(submissionId));
        emitter.onTimeout(() -> emitters.remove(submissionId));
        return emitter;
    }

    public void complete(String submissionId, SubmissionResponseDto payload) {
        SseEmitter emitter = emitters.remove(submissionId);
        if (emitter == null) {
            return;
        }
        try {
            emitter.send(SseEmitter.event().name("verdict").data(payload));
            emitter.complete();
        } catch (IOException e) {
            log.warn("Failed to push SSE verdict for {}", submissionId, e);
            emitter.completeWithError(e);
        }
    }
}
```

- [ ] **Step 4: Chạy test — kỳ vọng PASS**

Run: `cd judge-api && ./mvnw -q test -Dtest=SubmissionSseRegistryTest`
Expected: PASS (2 tests)

- [ ] **Step 5: Thêm SSE endpoint vào `SubmissionController.java`**

Thêm field `private final SubmissionSseRegistry sseRegistry;` (đã có `@RequiredArgsConstructor`) và method:

```java
    @org.springframework.web.bind.annotation.GetMapping(value = "/{id}/stream",
            produces = org.springframework.http.MediaType.TEXT_EVENT_STREAM_VALUE)
    @Operation(summary = "Stream the verdict for a submission via SSE")
    public org.springframework.web.servlet.mvc.method.annotation.SseEmitter stream(
            @PathVariable String id) {
        log.info("SSE subscribe for submission {}", id);
        return sseRegistry.subscribe(id);
    }
```

> Client có thể subscribe endpoint này ngay sau khi nhận `PENDING` từ `POST /submissions`. `SecurityConfig` cần cho phép request đã xác thực tới `/api/v1/submissions/*/stream` (JWT filter hiện có áp dụng như các route submissions khác — không cần đổi trừ khi có allowlist tường minh).

- [ ] **Step 6: Wire emit vào `JudgeResultConsumer.java`**

Thêm dependencies và emit sau khi save:

```java
// thêm import
import vn.thanhtuanle.submission.SubmissionSseRegistry;
import vn.thanhtuanle.submission.mapper.SubmissionMapper;
// thêm fields (constructor injection qua @RequiredArgsConstructor)
    private final SubmissionSseRegistry sseRegistry;
    private final SubmissionMapper submissionMapper;
// ở cuối onJudged(), NGAY SAU submissionRepository.save(submission):
        sseRegistry.complete(event.getSubmissionId(), submissionMapper.toDto(submission));
```

- [ ] **Step 7: Chạy toàn bộ test judge-api — kỳ vọng PASS**

Run: `cd judge-api && ./mvnw -q test`
Expected: BUILD SUCCESS (tất cả test cũ + mới pass)

- [ ] **Step 8: Commit**

```bash
cd judge-api
git add src/main/java/vn/thanhtuanle/submission/SubmissionSseRegistry.java \
        src/test/java/vn/thanhtuanle/submission/SubmissionSseRegistryTest.java \
        src/main/java/vn/thanhtuanle/submission/SubmissionController.java \
        src/main/java/vn/thanhtuanle/messaging/JudgeResultConsumer.java
git commit -m "feat: SSE endpoint pushes verdict when judging completes"
```

---

## Phase C — Hạ tầng & E2E

### Task 11: docker-compose — Kafka + judge-worker + wiring

**Files:**
- Modify: `judge-server/docker-compose.yml`

**Interfaces:** không có interface code.

- [ ] **Step 1: Cập nhật `judge-server/docker-compose.yml`**

```yaml
services:
  # NOTE: bitnami/kafka:3.7 was originally planned but Bitnami discontinued its
  # free-tier Docker Hub distribution (image now returns `manifest unknown`).
  # Replaced at implementation time with the official apache/kafka image, KRaft
  # mode, dual listener (INTERNAL for in-compose clients, EXTERNAL for host).
  kafka:
    image: apache/kafka:3.9.0
    container_name: kafka
    ports:
      - "9092:9092"
    environment:
      KAFKA_NODE_ID: 1
      KAFKA_PROCESS_ROLES: broker,controller
      KAFKA_LISTENERS: INTERNAL://:29092,CONTROLLER://:9093,EXTERNAL://:9092
      KAFKA_ADVERTISED_LISTENERS: INTERNAL://kafka:29092,EXTERNAL://localhost:9092
      KAFKA_LISTENER_SECURITY_PROTOCOL_MAP: CONTROLLER:PLAINTEXT,INTERNAL:PLAINTEXT,EXTERNAL:PLAINTEXT
      KAFKA_CONTROLLER_QUORUM_VOTERS: 1@kafka:9093
      KAFKA_CONTROLLER_LISTENER_NAMES: CONTROLLER
      KAFKA_INTER_BROKER_LISTENER_NAME: INTERNAL
      KAFKA_OFFSETS_TOPIC_REPLICATION_FACTOR: 1
      KAFKA_TRANSACTION_STATE_LOG_REPLICATION_FACTOR: 1
      KAFKA_TRANSACTION_STATE_LOG_MIN_ISR: 1
      KAFKA_GROUP_INITIAL_REBALANCE_DELAY_MS: 0
      KAFKA_AUTO_CREATE_TOPICS_ENABLE: "true"

  judge-server:
    image: qduoj/judge-server
    container_name: judge-server
    restart: always
    privileged: true
    environment:
      - TOKEN=123456
      - SERVICE_URL=http://judge-server:8080
      - BACKEND_URL=http://judge-api:8080/api/judge_server_heartbeat/
    ports:
      - "8080:8080"
    volumes:
      - ../judge-api/src/main/resources/test_cases:/test_case:ro
      - ../judge-api/log:/log

  judge-worker:
    build: ../judge-worker
    container_name: judge-worker
    restart: always
    depends_on:
      - kafka
      - judge-server
    environment:
      - KAFKA_BROKERS=kafka:9092
      - JUDGE_SERVER_URL=http://judge-server:8080
      - JUDGE_SERVER_TOKEN=123456
```

> judge-api chạy riêng (IDE hoặc container) cần `KAFKA_BROKERS=localhost:9092` (nếu chạy ngoài Docker) hoặc `kafka:9092` (nếu trong cùng compose network). `JUDGE_SERVER_TOKEN` của worker phải khớp `TOKEN` của judge-server.

- [ ] **Step 2: Khởi động stack hạ tầng — kỳ vọng thành công**

Run: `cd /home/tuan/Project/my-oj/judge-server && docker compose up -d kafka judge-server judge-worker`
Expected: 3 container `Up`. Kiểm tra: `docker compose ps` và `docker compose logs judge-worker | tail` thấy "judge-worker starting".

- [ ] **Step 3: Commit**

```bash
cd /home/tuan/Project/my-oj/judge-server
git add docker-compose.yml
git commit -m "chore: add kafka and judge-worker to docker-compose"
```

---

### Task 12: End-to-end verification (manual)

**Files:** không tạo/sửa file — kịch bản kiểm chứng.

**Interfaces:** không có.

- [ ] **Step 1: Chuẩn bị test case + info cho một problem**

Run: `cd /home/tuan/Project/my-oj/judge-server && python generate_info.py simple-a-plus-b`
Expected: `info file generated at .../test_cases/simple-a-plus-b/info` (đảm bảo có file `.in`/`.out` trong thư mục đó trước).

- [ ] **Step 2: Chạy toàn bộ stack (kafka, judge-server, judge-worker) + judge-api + postgres**

Run: `docker compose up -d` (judge-server compose) và khởi động judge-api (trỏ `KAFKA_BROKERS` tới broker đang chạy). Đảm bảo problem `simple-a-plus-b` + language tồn tại trong DB.

- [ ] **Step 3: Submit một bài đúng và xác nhận không block**

```bash
curl -s -X POST http://localhost:8080/api/v1/submissions \
  -H "Authorization: Bearer <JWT>" -H "Content-Type: application/json" \
  -d '{"sourceCode":"...AC solution...","languageIdentifier":"cpp","problemSlug":"simple-a-plus-b","shareSubmission":false}'
```
Expected: trả về **ngay lập tức** với `status = 6` (PENDING) — KHÔNG chờ chấm xong.

- [ ] **Step 4: Xác nhận verdict cập nhật qua Kafka**

Run: `curl -s http://localhost:8080/api/v1/submissions/<id> -H "Authorization: Bearer <JWT>"`
Expected: sau ~1-3s, `status = 0` (ACCEPTED), `cpuTime`/`memory` > 0. Kiểm tra `docker compose logs judge-worker` thấy publish `submission.judged`.

- [ ] **Step 5: (Tùy chọn) Xác nhận SSE đẩy verdict**

Run: `curl -N http://localhost:8080/api/v1/submissions/<new-id>/stream -H "Authorization: Bearer <JWT>"` (mở SSE) rồi submit `<new-id>` ở terminal khác.
Expected: nhận event `verdict` với payload chứa `status` cuối cùng, rồi stream đóng.

- [ ] **Step 6: Xác nhận idempotency (kill/restart worker)**

Chạy `docker compose restart judge-worker` giữa lúc có submission PENDING; submit lại vài bài. Expected: mỗi submission có đúng một verdict cuối; log judge-api thấy dòng "already finished ... ignoring" nếu có message trùng — KHÔNG có verdict bị ghi đè sai.

---

## Self-review notes

- **Spec coverage:** §2 kiến trúc → Task 8/9/10/11; §3 topics/schema → Task 7; §4 judge-api → Task 8/9/10; §5 worker → Task 1-6; §6 reliability (at-least-once + idempotency) → Task 5 (commit-after-publish) + Task 9 (PENDING guard); §7 infra → Task 11; §8 testing → test ở mỗi task + Task 12 E2E; §9 out-of-scope không có task (đúng chủ ý).
- **Type consistency:** `buildRequestedEvent` (Task 8) khớp `SubmissionRequestedEvent` (Task 7); `SubmissionJudgedEvent` field (`cpuTime`/`realTime`) khớp cách set trong consumer (Task 9); verdict codes Python (Task 1/2) khớp enum Java `SubmissionResult`.
- **Idempotency guard** dùng `status == PENDING(6)` nhất quán giữa consumer (Task 9) và test.
