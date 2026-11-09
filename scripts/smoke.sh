#!/usr/bin/env bash
# Smoke test of the shipped image: starts `serve` the way compose will, over mutual TLS, and talks to it.
#
#   1. builds the runtime image (the one that ships, not the test image);
#   2. writes a throw-away PKI with internal/testutil/cmd/mktls (the server's names include the container's);
#   3. runs the server with the production privileges (six capabilities, read-only root, /work tmpfs), under the
#      image's own tini and no `--init` (a k3s pod has none), and checks that tini is PID 1;
#   4. Ping with grpcurl and the judge-worker client certificate (and without the token, and Judge with a
#      stranger's certificate, which must be refused);
#   5. two real Judge calls with inline cases: a C++ a+b program (filter c_cpp), and the Java ac.java of the corpus
#      (a row with no seccomp filter, "") under the /tmp the server closed to 0711, where HotSpot cannot create hsperfdata;
#   6. `judge-server healthcheck` inside the container exits 0;
#   7. `docker stop` ends the server with exit status 0.
#
# Needs the proto of oj-common (PROTO_DIR, default ../oj-common/src/main/proto) for grpcurl. Exit status is
# non-zero when a step fails.
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT=$PWD
PROTO_DIR="${PROTO_DIR:-$ROOT/../oj-common/src/main/proto}"
IMAGE=judge-server:runtime
NAME=judge-server-smoke   # container name, host name and a name in the server certificate
NET=judge-smoke-net
TOKEN=smoke
CLIENT_CN=judge-worker
# Pinned: the client the smoke test talks with must not move under it. The Go image only runs mktls;
# the Makefile passes its GO_IMAGE so that both use one toolchain tag.
GRPCURL=fullstorydev/grpcurl:v1.9.3
GO_IMAGE="${GO_IMAGE:-golang:1.24}"

[ -f "$PROTO_DIR/judge_server.proto" ] || { echo "judge_server.proto not found in $PROTO_DIR (set PROTO_DIR)" >&2; exit 2; }

tls=$(mktemp -d)
cleanup() {
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
}
cleanup # what an interrupted earlier run left behind
trap 'cleanup; rm -rf "$tls"' EXIT

step() { printf '\n== %s\n' "$*"; }

step "build $IMAGE"
docker build --target runtime -t "$IMAGE" .
docker image ls "$IMAGE" --format 'image {{.Repository}}:{{.Tag}}  {{.ID}}  size {{.Size}}'

step "throw-away PKI (server names: judge-server, localhost, $NAME)"
docker run --rm -v "$ROOT":/src -v judge-server-gomod:/go/pkg/mod -v "$tls":/tls -w /src \
  -e GOFLAGS=-mod=mod -e GOCACHE=/tmp/gocache "$GO_IMAGE" \
  sh -c 'go run ./internal/testutil/cmd/mktls /tls "$1" && chmod a+rx /tls && chmod a+r /tls/*' sh "$NAME"
# The chmod is done there, as root, because the files are root's: on a Linux host the user running this script cannot
# chmod them, and mktemp made $tls 0700. The grpcurl container runs as another user (uid 100) and has to read them.

step "start $NAME"
docker network create "$NET" >/dev/null
docker run -d --name "$NAME" --hostname "$NAME" --network "$NET" \
  --cap-drop ALL \
  --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add FOWNER --cap-add SETGID --cap-add SETUID --cap-add KILL \
  --security-opt no-new-privileges:true \
  --read-only --tmpfs /work:exec,mode=755,size=512m --tmpfs /tmp:size=64m \
  --memory 1536m --cpus 2 \
  -e TOKEN="$TOKEN" -e JUDGE_ALLOWED_CLIENTS="$CLIENT_CN" \
  -v "$tls":/etc/oj/tls:ro \
  "$IMAGE" >/dev/null

# The server warms the Go build cache before it listens, so give it time.
up=
for _ in $(seq 1 60); do
  if docker exec "$NAME" /usr/local/bin/judge-server healthcheck >/dev/null 2>&1; then up=1; break; fi
  if [ "$(docker inspect -f '{{.State.Running}}' "$NAME")" != true ]; then break; fi
  sleep 1
done
if [ -z "$up" ]; then
  echo "the server did not come up; its log:" >&2
  docker logs "$NAME" >&2
  exit 1
fi
docker logs "$NAME" 2>&1 | sed 's/^/  server: /' | tail -5

# No `--init` above: the image's own tini is PID 1, which reaps what a sweep or a group kill leaves behind.
[ "$(docker exec "$NAME" cat /proc/1/comm)" = tini ] || { echo "PID 1 of the container is not tini" >&2; exit 1; }

# sha256 of the token, the value of the x-judge-server-token header
token_hash=$(printf %s "$TOKEN" | { sha256sum 2>/dev/null || shasum -a 256; } | cut -d' ' -f1)

grpcurl() { # grpcurl <client cert name> <extra args...> -- <method> [json]
  local cn=$1; shift
  docker run --rm --network "$NET" -v "$tls":/tls:ro -v "$PROTO_DIR":/proto:ro "$GRPCURL" \
    -cacert /tls/ca.crt -cert "/tls/$cn.crt" -key "/tls/$cn.key" \
    -import-path /proto -proto judge_server.proto "$@"
}

step "Ping as $CLIENT_CN with the token"
out=$(grpcurl "$CLIENT_CN" -H "x-judge-server-token: $token_hash" "$NAME:8080" oj.judge.v1.JudgeServer/Ping)
echo "$out"
echo "$out" | grep -q "\"hostname\": \"$NAME\"" || { echo "Ping: wrong or no hostname" >&2; exit 1; }
echo "$out" | grep -q '"maxJobs": 2' || { echo "Ping: maxJobs is not 2" >&2; exit 1; }

step "Ping without the token must be refused (Unauthenticated)"
if out=$(grpcurl "$CLIENT_CN" "$NAME:8080" oj.judge.v1.JudgeServer/Ping 2>&1); then
  echo "Ping without a token was answered: $out" >&2; exit 1
fi
echo "$out"
echo "$out" | grep -q Unauthenticated || { echo "want Unauthenticated" >&2; exit 1; }

step "Judge as stranger (CA-signed, not in JUDGE_ALLOWED_CLIENTS) must be refused (PermissionDenied; Ping admits any CA-signed client, for the healthcheck)"
if out=$(grpcurl stranger -H "x-judge-server-token: $token_hash" -d '{}' "$NAME:8080" oj.judge.v1.JudgeServer/Judge 2>&1); then
  echo "Judge as a stranger was answered: $out" >&2; exit 1
fi
echo "$out"
echo "$out" | grep -q PermissionDenied || { echo "want PermissionDenied" >&2; exit 1; }

step "Judge: C++ a+b, three inline cases, through the real sandbox"
req=$(cat <<'JSON'
{
  "src": "#include <iostream>\nint main() { long long a, b; std::cin >> a >> b; std::cout << a + b << \"\\n\"; }\n",
  "languageConfig": {
    "compile": {"srcName": "main.cpp", "exeName": "main", "maxCpuTimeMs": 3000, "maxRealTimeMs": 5000,
                "maxMemoryBytes": 1073741824,
                "compileCommand": "/usr/bin/g++ -DONLINE_JUDGE -O2 -w -fmax-errors=3 -std=c++17 {src_path} -lm -o {exe_path}"},
    "run": {"command": "{exe_path}", "seccompRule": "c_cpp"}
  },
  "maxCpuTimeMs": 1000,
  "maxMemoryBytes": 134217728,
  "output": true,
  "testCase": {"cases": [
    {"input": "1 2\n", "output": "3\n"},
    {"input": "10 20\n", "output": "30\n"},
    {"input": "-5 5\n", "output": "0\n"}
  ]}
}
JSON
)
out=$(grpcurl "$CLIENT_CN" -H "x-judge-server-token: $token_hash" -d "$req" "$NAME:8080" oj.judge.v1.JudgeServer/Judge)
echo "$out"
# grpcurl leaves out fields that are zero: an AC case has no "result".
[ "$(echo "$out" | grep -c '"testCase"')" = 3 ] || { echo "Judge: want 3 case results" >&2; exit 1; }
if echo "$out" | grep -q '"result"'; then echo "Judge: a case is not AC" >&2; exit 1; fi

step "Judge: Java ac.java of the corpus, a row with no seccomp filter, with /tmp closed to 0711"
# The program and the row of the engine tests (V2 + V9), minus the two security manager flags: the policy file is a
# compose mount, not part of the image. /tmp is the tmpfs above, which the server closed to 0711 at start, so HotSpot
# cannot create /tmp/hsperfdata_<user> (its path is compiled in) and has to run without it.
java_src=$(sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' "$ROOT/testdata/corpus/java/ac.java" | awk '{ printf "%s%s", $0, "\\n" }')
req=$(cat <<JSON
{
  "src": "$java_src",
  "languageConfig": {
    "compile": {"srcName": "Main.java", "exeName": "Main", "maxCpuTimeMs": 3000, "maxRealTimeMs": 5000,
                "maxMemoryBytes": -1,
                "compileCommand": "/usr/bin/javac {src_path} -d {exe_dir} -encoding UTF8"},
    "run": {"command": "/usr/bin/java -cp {exe_dir} -Xss1M -Xmx256m -Djava.awt.headless=true -Dfile.encoding=UTF-8 Main",
            "seccompRule": "", "memoryLimitCheckOnly": true}
  },
  "maxCpuTimeMs": 1000,
  "maxMemoryBytes": 268435456,
  "output": true,
  "testCase": {"cases": [
    {"input": "1 2\\n", "output": "3\\n"},
    {"input": "10 20\\n", "output": "30\\n"},
    {"input": "-5 5\\n", "output": "0\\n"}
  ]}
}
JSON
)
out=$(grpcurl "$CLIENT_CN" -H "x-judge-server-token: $token_hash" -d "$req" "$NAME:8080" oj.judge.v1.JudgeServer/Judge)
echo "$out"
[ "$(echo "$out" | grep -c '"testCase"')" = 3 ] || { echo "Judge (Java): want 3 case results" >&2; exit 1; }
if echo "$out" | grep -q '"result"'; then echo "Judge (Java): a case is not AC" >&2; exit 1; fi
# The closed /tmp is what the call above ran under: mode 0711 and no perf-data directory in it.
[ "$(docker exec "$NAME" stat -c %a /tmp)" = 711 ] || { echo "/tmp is not 0711" >&2; exit 1; }
if docker exec "$NAME" sh -c 'ls -d /tmp/hsperfdata_*' >/dev/null 2>&1; then
  echo "a JVM created hsperfdata in /tmp, which should have been closed" >&2; exit 1
fi

step "healthcheck inside the container"
docker exec "$NAME" /usr/local/bin/judge-server healthcheck
echo "exit status $?"

step "docker stop: exit status of the server"
docker stop -t 30 "$NAME" >/dev/null
status=$(docker inspect -f '{{.State.ExitCode}}' "$NAME")
echo "exit status $status"
[ "$status" = 0 ] || { docker logs "$NAME" >&2; exit 1; }

printf '\nSMOKE OK\n'
