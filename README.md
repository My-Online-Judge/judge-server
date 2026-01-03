# Judge Server

This is the Judge Server component for the Online Judge system. It runs in a Docker container and executes user submissions.

## Prerequisites

- Docker
- Docker Compose

## Configuration

The configuration is managed via `docker-compose.yml`. The following environment variables are required:

- `TOKEN`: The token used for authentication between the backend and the judge server.
- `SERVICE_URL`: The URL of this judge server (usually `http://localhost:8080` or accessible IP).
- `BACKEND_URL`: The URL of the Online Judge Backend to report heartbeats to.

**Example `docker-compose.yml`**:
```yaml
services:
  judge-server:
    image: registry.cn-hangzhou.aliyuncs.com/onlinejudge/judge_server
    container_name: judge-server
    restart: always
    privileged: true
    environment:
      - TOKEN=YOUR_TOKEN_HERE
      - SERVICE_URL=http://localhost:8000
      - BACKEND_URL=http://localhost:8000/api/judge_server_heartbeat/
    ports:
      - "8080:8080"
    volumes:
      - ./test_cases:/test_case:ro
      - ./log:/log
```

## Running the Server

To start the server, run:

```bash
docker compose up -d
```

To stop the server:

```bash
docker compose down
```

## Verification

### 1. Check Container Status
Ensure the container is running:
```bash
docker ps
```
*Note: The status might be `unhealthy` if `BACKEND_URL` is not reachable, but the server itself should be `Up`.*

### 2. Verify API Responsiveness
You can check if the server is responsive by pinging it.
**Important**: You must provide the `Content-Type` header and a JSON body (even empty), otherwise the server will error.

```bash
curl -X POST http://localhost:8080/ping \
     -H "Content-Type: application/json" \
     -d "{}" \
     -H "X-Judge-Server-Token: YOUR_HASHED_TOKEN_HERE"
```

Example output:
```json
{"err": null, "data": {"hostname": "...", "action": "pong"}}
```

## Troubleshooting

- **Worker failed to boot**: Check `log/gunicorn.log`. usually caused by missing environment variables.
- **Connection refused (BACKEND_URL)**: Ensure `BACKEND_URL` in `docker-compose.yml` points to a reachable address. If running locally, you might need to use `host.docker.internal` or your machine's IP instead of `localhost`.
