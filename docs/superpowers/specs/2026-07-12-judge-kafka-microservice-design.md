# Design: Tách Judge thành microservice qua Kafka

- **Ngày:** 2026-07-12
- **Trạng thái:** Approved (chờ implementation plan)
- **Repo chứa spec:** `judge-server` (hub hạ tầng: docker-compose, README)
- **Phạm vi liên repo:** `judge-api` (Spring Boot), `judge-portal` (frontend), `judge-worker` (Python — mới), `judge-server` (docker-compose + qduoj judge-server)

## 1. Mục tiêu & bối cảnh

### Mục tiêu
Bỏ lời gọi HTTP **đồng bộ (blocking)** từ backend tới judge server; thay bằng giao tiếp **event-driven qua Kafka**. Judge trở thành một **service độc lập**. Ưu tiên: **decouple + async** (submit không block, kết quả cập nhật sau).

### Kiến trúc hiện tại (monolith + gọi judge đồng bộ)
- `judge-api` (Spring Boot 3.4.1, Postgres/JPA, Spring Security JWT) là monolith: auth, user, problem, language, submission, judge-server registry.
- Khi user submit (`POST /submissions` → `SubmissionService.submit()`):
  1. `judgeService.validate()` — chặn keyword blacklist + độ dài code.
  2. Tạo `Submission` status `PENDING`, lưu DB.
  3. **Gọi HTTP đồng bộ** (`JudgeService.judge()`, `RestClient`) tới qduoj `judge-server` (Python, port 8080) endpoint `/judge`, header `X-Judge-Server-Token: sha256(token)`.
  4. `processJudgeResponse()` / `resolveVerdict()` xử lý kết quả per-testcase → set verdict/stats → lưu.
  5. Trả `SubmissionResponseDto` đầy đủ. → **HTTP request block đến khi chấm xong.**
- `judge-server`: image bên thứ ba (qduoj) — API `/judge` đồng bộ, **không sửa được** để tự đọc Kafka. Test cases mount qua volume vào `/test_case`.
- `judge-portal`: frontend.

### Vấn đề
- Request submit block → trải nghiệm kém, tài nguyên web thread bị giữ.
- Backend và judging bị coupling chặt qua HTTP đồng bộ.

## 2. Kiến trúc mục tiêu

```
                 (JWT, đồng bộ)                    submission.requested
   judge-portal ───────────────► judge-api ─────────────────────────────►  judge-worker (Python)
        ▲                        (Spring Boot)                                     │
        │  SSE push verdict           ▲   │                                        │ HTTP POST /judge
        └─────────────────────────────┘   │ submission.judged                      ▼  (X-Judge-Server-Token)
                                          └─────────────────────────────────  qduoj judge-server
                                                                              (+ test_cases volume)
```

- **judge-api**: `submit()` → tạo `PENDING`, lưu DB, **publish `submission.requested`**, trả `PENDING` ngay (không block). Consume `submission.judged` → cập nhật DB → **đẩy SSE**.
- **judge-worker (Python, mới)**: consume `submission.requested` → gọi qduoj judge-server → **resolve verdict + gộp stats (toàn bộ logic judge chuyển sang đây)** → publish `submission.judged`.

### Quyết định thiết kế chính
| Quyết định | Chọn | Lý do |
|---|---|---|
| Cơ chế giao tiếp | Kafka event-driven | Decouple + async |
| Client nhận verdict | **SSE push** | Verdict đẩy một chiều server→client; nhẹ hơn WebSocket; dùng lại JWT |
| Stack worker | **Python** (mô phỏng qduoj dispatcher) | Chuyển toàn bộ logic judge sang Python; bám sát qduoj |
| Vị trí resolve verdict | **Trong worker (Python)** | "Chuyển toàn bộ logic judge sang Python" → judge-api chỉ persist |
| Ranh giới dữ liệu worker | **Stateless, event tự chứa đủ** | judge-api (có DB) đóng gói language_config + limits vào event; worker là hàm thuần của event; dễ scale |
| Reliability | **Cơ bản** (at-least-once + idempotency) | Không mất/nhân đôi submission; giữ spec gọn |
| Kafka lib (Python) | **kafka-python** | Nhẹ, đủ dùng |
| Kafka deployment | **KRaft, single broker** (không Zookeeper) | Đơn giản cho dev |

**Phương án đã loại:** worker "mỏng" publish raw response còn judge-api tự resolve verdict (Java). Loại vì yêu cầu là chuyển *toàn bộ* logic judge sang Python.

## 3. Kafka topics & event schema

| Topic | Key | Producer → Consumer (group) | Payload |
|---|---|---|---|
| `submission.requested` | `submissionId` | judge-api → worker (`judge-workers`) | `submissionId`, `src`, `language_config`, `max_cpu_time`, `max_memory`, `test_case_id`, `output` |
| `submission.judged` | `submissionId` | worker → judge-api (`judge-api-results`) | `submissionId`, `status`, `result`, `cpu_time`, `real_time`, `memory`, `error_message`, `details[]` |

- Key = `submissionId` → giữ thứ tự per-submission, cho phép scale worker theo partition.
- `language_config` giữ nguyên cấu trúc `JudgeLanguageConfigDto` hiện tại (compile + run config).
- `details[]` = mảng per-testcase theo `JudgeResultDto` (cpu_time, real_time, memory, signal, exit_code, error, result, test_case, output_md5, output).

### `submission.requested` (ví dụ)
```json
{
  "submissionId": "uuid",
  "src": "…source code…",
  "language_config": { "compile": { … }, "run": { … } },
  "max_cpu_time": 1000,
  "max_memory": 268435456,
  "test_case_id": "problem-slug",
  "output": true
}
```

### `submission.judged` (ví dụ)
```json
{
  "submissionId": "uuid",
  "status": 0,
  "result": 0,
  "cpu_time": 12,
  "real_time": 15,
  "memory": 3072000,
  "error_message": null,
  "details": [ { "cpu_time": 12, "result": 0, "error": 0, "test_case": "1", … } ]
}
```

## 4. judge-api (Spring Boot) — thay đổi

**Thêm:** dependency `spring-kafka`; config `spring.kafka.*` (bootstrap-servers, consumer group `judge-api-results`, JSON serde); tên topics ra config.

**`SubmissionService.submit()`** (sửa):
- Giữ `judgeService.validate()` (validation tại API boundary).
- Tạo `Submission` `PENDING`, lưu DB.
- Build payload `submission.requested` (dùng lại logic dựng `JudgeSubmissionDto` + `language_config` từ `Problem`/`Language`).
- Publish qua `KafkaTemplate`, key = submissionId.
- Trả `SubmissionResponseDto` trạng thái `PENDING` **ngay** (bỏ lời gọi `judgeService.judge()` đồng bộ).

**`JudgeService`** (sửa): giữ `validate()` + logic build request/`language_config`; **xóa** `judge()` (HTTP call). `processJudgeResponse()`/`resolveVerdict()` **xóa** khỏi judge-api (đã chuyển sang Python).

**Mới — `JudgeResultConsumer`** (`@KafkaListener` trên `submission.judged`):
- Load `Submission` theo `submissionId`.
- **Idempotency:** chỉ apply nếu `status == PENDING`; nếu đã finished → log & bỏ qua (an toàn với reprocess).
- Set `status`, `result`, `cpuTime`, `time`, `memory`, `errorMessage` + lưu `details`, save.
- Emit SSE tới emitter đang subscribe submission đó.

**Mới — SSE endpoint** `GET /submissions/{id}/stream`:
- Trả `SseEmitter`; registry in-memory `Map<submissionId, SseEmitter>` (giả định 1 instance judge-api).
- `JudgeResultConsumer` gọi emit khi verdict tới; complete emitter sau khi gửi.
- Bảo vệ bằng JWT filter sẵn có.

**Giữ nguyên:** validate rules, enum `SubmissionResult`/`SubmissionError`, mapper, các GET submission (portal có thể fallback polling nếu cần).

## 5. judge-worker (Python) — service mới `judge-worker/`

Mô phỏng `DispatcherBase` / `JudgeDispatcher.judge()` của qduoj.

```
judge-worker/
  config.py       # env: KAFKA_BROKERS, JUDGE_SERVER_URL, JUDGE_SERVER_TOKEN, topic names, group id
  models.py       # dataclass cho 2 event schema (requested / judged)
  dispatcher.py   # build payload, POST {JUDGE_SERVER_URL}/judge, header X-Judge-Server-Token=sha256(token)
  verdict.py      # resolve verdict (ACM) + gộp stats — parity với logic Java hiện tại
  consumer.py     # vòng lặp consume → dispatch → verdict → publish → commit
  main.py         # entrypoint
  requirements.txt  # kafka-python, requests
  Dockerfile
  tests/
```

**Luồng xử lý một message:**
1. Consume `submission.requested`.
2. `dispatcher`: POST payload tới judge-server `/judge` (token sha256).
3. Parse response:
   - Envelope error (`{"err": "...", "data": ...}`): `CompileError`/`SPJCompileError` → `COMPILE_ERROR`; còn lại → `SYSTEM_ERROR`.
   - Data là mảng per-testcase → `verdict.resolve()`.
4. `verdict.resolve()` (ACM-mode, parity với `resolveVerdict` Java):
   - Sandbox error bất kỳ (`error != 0`) → `SYSTEM_ERROR` (override tất cả).
   - Ngược lại: testcase fail **đầu tiên** (`result != 0`) thắng; all-pass → `ACCEPTED`.
   - Gộp `max` cpu_time / real_time / memory.
5. Publish `submission.judged` (key = submissionId).
6. **Commit offset sau khi publish** (at-least-once).

**Enum verdict:** định nghĩa hằng số Python khớp `SubmissionResult` của judge-api (ACCEPTED=0, WRONG_ANSWER=-1, CPU_TLE=1, REAL_TLE=2, MLE=3, RUNTIME_ERROR=4, SYSTEM_ERROR=5, COMPILE_ERROR, PENDING). Giá trị lấy từ enum Java hiện có để đảm bảo khớp DB.

## 6. Reliability (mức cơ bản)

- **At-least-once:** worker commit offset *sau* khi publish `submission.judged`.
- **Idempotency:** judge-api result consumer chỉ apply nếu submission còn `PENDING`; duplicate (đã finished) → bỏ qua. → reprocess an toàn.
- **Worker gặp lỗi khi gọi judge-server** (exception/timeout): publish verdict `SYSTEM_ERROR` (submission không kẹt `PENDING` mãi), rồi commit.
- **Không** làm: DLQ, retry backoff (ghi nhận cho tương lai).

## 7. Hạ tầng (docker-compose)

Cập nhật `judge-server/docker-compose.yml` (và/hoặc `my-oj/docker-compose.integration.yml`):
- Thêm **Kafka** (KRaft, single broker, không Zookeeper).
- Services: `postgres`, `kafka`, `judge-api`, **`judge-worker`**, `judge-server` (qduoj), `judge-portal`.
- `test_cases` vẫn mount vào `judge-server` (không đổi).
- Biến môi trường worker: `KAFKA_BROKERS`, `JUDGE_SERVER_URL`, `JUDGE_SERVER_TOKEN`.
- judge-api thêm `spring.kafka.bootstrap-servers`.

## 8. Testing

**judge-api:**
- Unit: `submit()` publish đúng event (mock `KafkaTemplate`) + trả `PENDING`.
- Unit: `JudgeResultConsumer` apply verdict/stats; idempotency (đã finished → bỏ qua).
- Integration: embedded Kafka / Testcontainers cho vòng producer→consumer.

**judge-worker (Python):**
- Unit: `dispatcher` dựng payload đúng (token sha256, field mapping).
- **Parity test `verdict.resolve()`**: all-pass→ACCEPTED; first-fail thắng; sandbox error→SYSTEM_ERROR; envelope compile error→COMPILE_ERROR.
- Integration (optional): fake judge-server HTTP + embedded kafka.

**End-to-end:** `docker-compose.integration` + `mock-judge-server`: submit → verdict về DB → SSE đẩy.

## 9. Ngoài phạm vi (làm sau)

- Gỡ bỏ `JudgeServer` heartbeat/registry trong judge-api (worker dùng URL cấu hình; registry không còn trên critical path — để lại nguyên, không dùng).
- SSE fan-out đa instance judge-api (cần Redis pub/sub) — hiện giả định 1 instance.
- DLQ / retry backoff.
- OI-mode / SPJ scoring & partial scoring.
- `generate_info.py` / sinh test-case `info` (thuộc luồng quản trị problem).
- Portal: nâng cấp UI để consume SSE (spec này chỉ cung cấp endpoint SSE; tích hợp UI có thể là plan riêng).

## 10. Tiêu chí thành công

1. `POST /submissions` trả `PENDING` **ngay lập tức**, không block chờ judge.
2. Submission được chấm bởi judge-worker Python (không còn HTTP đồng bộ trong judge-api) và DB cập nhật verdict cuối.
3. Verdict được đẩy tới client qua SSE.
4. Kill/restart worker giữa chừng không làm mất submission cũng không nhân đôi verdict (idempotency).
5. Test parity verdict pass (khớp hành vi ACM hiện tại).
6. `docker compose up` chạy được toàn stack: postgres + kafka + judge-api + judge-worker + judge-server (+ portal).
