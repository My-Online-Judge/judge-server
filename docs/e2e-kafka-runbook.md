# E2E Runbook — Kafka Judge Microservice (Task 12)

> **Legacy:** this runbook was written for the qduoj stack and its `docker-compose.yml`, which has been removed from this repo. For the current stack use `judge-deployment/docker-compose.yml`. The `docker compose` commands below no longer run as written.

Kiểm chứng end-to-end luồng mới: `submit → submission.requested (Kafka) → judge-worker → judge-server → submission.judged (Kafka) → judge-api cập nhật DB → SSE đẩy verdict`.

Chạy thủ công khi bạn có: Docker, một Postgres cho judge-api, và một JWT hợp lệ. Đánh dấu **[ENV]** ở những bước phụ thuộc môi trường của bạn.

---

## 0. Lưu ý quan trọng trước khi bắt đầu

- **AppArmor teardown:** Trên host này, profile `docker-default` đang chặn tín hiệu kill tới container **non-privileged**, nên `docker compose down` có thể **không dừng được** `kafka`/`judge-worker`. Nếu container kẹt, dọn bằng (cần root):
  ```
  ! sudo systemctl restart docker
  ```
  Cân nhắc sửa host (cập nhật/khôi phục profile AppArmor `docker-default`) trước khi chạy E2E nhiều lần.
- **Cổng judge-api:** profile `dev` chạy ở **:8000** (không phải 8080). judge-server chiếm 8080.
- judge-api chạy trên host kết nối Kafka qua **`localhost:9092`** (listener EXTERNAL). judge-worker trong compose dùng **`kafka:29092`** (listener INTERNAL).

---

## 1. Chuẩn bị test case + info

judge-server đọc test case từ volume `judge-api/src/main/resources/test_cases/<problem-slug>/`. Ví dụ problem `simple-a-plus-b`:

```bash
cd /home/tuan/Project/my-oj
ls judge-api/src/main/resources/test_cases/simple-a-plus-b/   # cần có 1.in, 1.out, ...
# sinh file info (md5 v.v.) mà judge-server yêu cầu:
cd judge-server && python generate_info.py simple-a-plus-b
# -> "info file generated at .../test_cases/simple-a-plus-b/info"
```
Nếu chưa có test case: tạo cặp `1.in`/`1.out` (input/output đúng của bài) rồi chạy lại `generate_info.py`.

---

## 2. Dựng hạ tầng (kafka + judge-server + judge-worker)

```bash
cd /home/tuan/Project/my-oj/judge-server
docker compose up -d kafka judge-server judge-worker
docker compose ps                         # cả 3 phải Up
docker exec kafka /opt/kafka/bin/kafka-topics.sh --bootstrap-server localhost:9092 --list
docker compose logs judge-worker | tail    # thấy "judge-worker starting" + joined group
```
Topics `submission.requested` / `submission.judged` sẽ auto-create khi có message đầu tiên (đã bật `KAFKA_AUTO_CREATE_TOPICS_ENABLE`).

> judge-server có thể hiện `(unhealthy)` khi judge-api chưa chạy — bình thường (nó heartbeat tới judge-api, nhưng luồng chấm mới KHÔNG phụ thuộc heartbeat).

---

## 3. Postgres cho judge-api  **[ENV]**

Dùng Postgres sẵn có của bạn. Chuẩn bị biến môi trường:
```bash
export DATABASE_URL="jdbc:postgresql://localhost:5432/oj"
export DATABASE_USERNAME="oj"
export DATABASE_PASSWORD="oj"
```
(OAuth2 Google vars — `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET`, `GOOGLE_REDIRECT_URI`, `GOOGLE_*_URI` — cần cho luồng đăng nhập ở bước 6.)

---

## 4. Chạy judge-api (host, profile dev, port 8000)

```bash
cd /home/tuan/Project/my-oj/judge-api
export KAFKA_BROKERS="localhost:9092"     # listener EXTERNAL của Kafka
./mvnw spring-boot:run -Dspring-boot.run.profiles=dev
```
Kiểm tra log khởi động thấy Kafka consumer container start (group `judge-api-results`, topic `submission.judged`). `ddl-auto: update` sẽ tạo/nâng schema.

---

## 5. Seed problem + language  **[ENV]**

Cần trong DB:
- **Problem** slug `simple-a-plus-b`: `timeLimit` (ms, → max_cpu_time), `memoryLimit` (MB, → *1MB thành bytes), status hợp lệ.
- **Language** `identifier` khớp bài nộp (vd `cpp`): `srcName`, `exeName`, `compileCommand`, `runCommand`, `seccompRule`, `compileMaxMemory`, `maxMemory` (mirror cấu hình QingdaoU — xem `JudgeService.buildRequestedEvent`).

Seed qua API admin (nếu có) hoặc SQL trực tiếp. **Quan trọng:** `problemSlug` phải trùng tên thư mục test case ở bước 1 (nó là `test_case_id` gửi cho judge-server).

---

## 6. Lấy JWT  **[ENV]**

Auth là Google OAuth2. Lấy Bearer JWT như frontend làm:
- `GET /api/v1/auth/outbound/google` → lấy URL đăng nhập Google;
- sau khi Google trả code, `POST /api/v1/auth/outbound/authentication` (`ExchangeTokenRequest`) → `AuthResponse` chứa access token (JWT).

Hoặc tái dùng token từ phiên trình duyệt đang đăng nhập. Lưu lại:
```bash
export JWT="<access-token>"
```

---

## 7. Submit — xác nhận KHÔNG block

```bash
curl -s -X POST http://localhost:8000/api/v1/submissions \
  -H "Authorization: Bearer $JWT" -H "Content-Type: application/json" \
  -d '{"sourceCode":"<code AC của bài>","languageIdentifier":"cpp","problemSlug":"simple-a-plus-b","shareSubmission":false}'
```
**Kỳ vọng:** trả về **ngay lập tức** với `"status": 6` (PENDING). Ghi lại `id` trả về → `export SID=<id>`.

✅ *Tiêu chí 1:* response tức thì, không chờ chấm.

---

## 8. Xác nhận verdict cập nhật qua Kafka

```bash
# xem worker xử lý:
docker compose -f /home/tuan/Project/my-oj/judge-server/docker-compose.yml logs judge-worker | tail -20
# poll submission:
curl -s http://localhost:8000/api/v1/submissions/$SID -H "Authorization: Bearer $JWT"
```
**Kỳ vọng:** sau ~1-3s, `status = 0` (ACCEPTED) với code đúng; `cpuTime`/`memory` > 0. Log worker thấy publish `submission.judged`. judge-api log thấy "Applied verdict ... to submission".

✅ *Tiêu chí 2:* submission được chấm bởi worker Python (không còn HTTP đồng bộ trong judge-api) và DB có verdict cuối.

---

## 9. Xác nhận SSE đẩy verdict

Terminal A (mở stream cho một submission MỚI trước khi submit):
```bash
curl -N http://localhost:8000/api/v1/submissions/$NEW_SID/stream -H "Authorization: Bearer $JWT"
```
Terminal B: submit bài với id tương ứng (thực tế: submit trước để lấy id, rồi mở stream ngay — cửa sổ PENDING vài giây là đủ).
**Kỳ vọng:** stream nhận event tên `verdict` với payload chứa `status` cuối, rồi đóng.

✅ *Tiêu chí 3:* verdict được đẩy qua SSE.

---

## 10. Xác nhận idempotency (replay an toàn)

Mô phỏng message trùng: dùng kafka console producer bơm lại một `submission.judged` cho một submission ĐÃ finished:
```bash
docker exec -i kafka /opt/kafka/bin/kafka-console-producer.sh \
  --bootstrap-server localhost:9092 --topic submission.judged \
  --property parse.key=true --property key.separator=: <<EOF
$SID:{"submissionId":"$SID","status":-1,"result":-1,"cpu_time":1,"real_time":1,"memory":1,"error_message":null,"details":[]}
EOF
```
**Kỳ vọng:** judge-api log "Submission ... already finished ... ignoring verdict"; `GET /submissions/$SID` **giữ nguyên** status ACCEPTED (không bị ghi đè WRONG_ANSWER).

✅ *Tiêu chí 4:* verdict trùng/replay không ghi đè kết quả đã có.

---

## 11. Dọn dẹp

```bash
cd /home/tuan/Project/my-oj/judge-server
docker compose down          # nếu container non-privileged kẹt -> xem Lưu ý 0
# dừng judge-api: Ctrl-C ở terminal spring-boot:run
```

---

## 12. UI verdict (SSE) — judge-portal

Frontend (`judge-portal`, nhánh `feature/sse-verdict-ui`) giờ tự cập nhật bảng Submissions theo thời gian thực qua SSE: `ProblemSubmit` emit submission vừa tạo → `ProblemDetailPage` chuyển tab và chèn dòng "Judging" lạc quan vào `ProblemSubmissions` (qua `addPendingSubmission`, `defineExpose`) → component mở stream (`useSubmissionStream().watch`) và cập nhật dòng tại chỗ khi verdict về, không cần reload.

> **Chưa được chạy live trong phiên phát triển này** — môi trường đó không có backend/DB/Kafka/đăng nhập chạy sẵn. Mục này ghi lại quy trình + tiêu chí để người vận hành chạy tay sau khi đã dựng đủ stack theo mục 2–6. Đã verify tự động bằng `npm run type-check` (0 lỗi) và `npm run test` (Vitest, 9 test pass — xem mục "Đã tự động verify sẵn" bên dưới); phần tương tác trình duyệt dưới đây vẫn cần chạy tay.

Chuẩn bị: dựng đủ stack theo mục 2–6 (kafka + judge-server + judge-worker + judge-api + Postgres + JWT hợp lệ), sau đó chạy judge-portal (`npm run dev`) với `VITE_API_BASE_URL` trỏ về judge-api (`http://localhost:8000/api/v1`).

Quy trình:
1. Đăng nhập vào judge-portal, mở một problem, vào tab **Submit**, nộp một bài AC.
2. Xác nhận app tự chuyển sang tab **Submissions** và một dòng mới xuất hiện **ngay lập tức** với spinner + badge *Pending*/*Judging*.
3. Trong vài giây, badge tự chuyển sang verdict cuối (AC/WA/…) kèm time/memory — **không cần reload thủ công**.
4. Nộp lại lần nữa và theo dõi dòng mới ngay lập tức: xác nhận trường hợp chấm nhanh (verdict về trước khi stream kịp mở) vẫn ra kết quả đúng, nhờ backend replay verdict terminal tại thời điểm subscribe (mục 9).
5. Mở DevTools → Network: request `/stream` phải **đóng lại ngay sau khi có verdict** (không có vòng lặp reconnect).

✅ Tiêu chí thành công:
- Dòng submission mới hiện ra ngay lập tức (optimistic, trước khi có verdict), có spinner khi trạng thái là PENDING/JUDGING.
- Badge tự chuyển sang verdict cuối cùng mà không cần reload trang.
- Request `/stream` đóng lại sau khi nhận verdict, không tự reconnect.
- Trường hợp verdict về nhanh hơn thời điểm mở stream (race điều kiện) vẫn được xử lý đúng nhờ replay từ backend.

Ghi lại kết quả (pass/fail + ghi chú) khi người vận hành thực hiện.

---

## Checklist tiêu chí thành công

- [ ] (7) `POST /submissions` trả PENDING ngay, không block.
- [ ] (8) Submission được chấm qua worker + Kafka, DB có verdict cuối.
- [ ] (9) Verdict đẩy qua SSE.
- [ ] (10) Replay `submission.judged` không ghi đè verdict đã finished (idempotency).
- [ ] (2) Toàn stack `docker compose up` chạy được (kafka + judge-server + judge-worker) + judge-api + postgres.
- [ ] (12) UI verdict SSE: dòng submission live-update đúng theo tiêu chí ở mục 12 (manual — chưa chạy trong phiên tự động).

## Đã tự động verify sẵn (không cần làm lại)

- judge-worker: 18 unit test (verdict parity, dispatcher, handler, commit-after-publish ordering) — pass.
- judge-api: unit test submit-publishes-PENDING, consumer idempotency (PENDING guard), SSE registry — pass. (`BackendServiceApplicationTests.contextLoads` cần Postgres — bỏ qua khi chạy offline.)
- judge-portal: `npm run type-check` (vue-tsc --build, 0 lỗi) và `npm run test` (Vitest — `isTerminalStatus` + `useSubmissionStream`, 9 test pass). Verify live theo mục 12 vẫn cần chạy tay (browser + full stack).
- Runtime: apache/kafka:3.9.0 Up healthy; judge-worker kết nối `kafka:29092`, join consumer group, không crash-loop.
