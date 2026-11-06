#!/usr/bin/env bash
# End-to-end check of the quickstart: bring up the docker-compose stack with
# the mock supplier, create an API key, then provision, SSH into and
# terminate an instance through the generic adapter.
#
# Usage, from the repository root:
#   scripts/e2e.sh            # leaves the stack running afterwards
#   E2E_DOWN=1 scripts/e2e.sh # tears the stack down at the end
#
# Needs Docker with the compose plugin, curl, python3 and ssh.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
DC="docker compose -f $ROOT/deploy/docker-compose.yml --profile mock"
API=${E2E_API:-http://127.0.0.1:8080}
MOCK=${E2E_MOCK:-http://127.0.0.1:8081}
WORK=$(mktemp -d)
json() { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }

cleanup() {
  status=$?
  if [ "$status" -ne 0 ]; then
    echo "== FAILED; recent gpuctl and mock-supplier logs"
    $DC logs --tail 80 gpuctl mock-supplier || true
  fi
  if [ "${E2E_DOWN:-0}" = 1 ]; then $DC down -v --remove-orphans >/dev/null 2>&1 || true; fi
  rm -rf "$WORK"
}
trap cleanup EXIT

echo "== reset and start the stack"
$DC down -v --remove-orphans >/dev/null 2>&1 || true
$DC up -d --build
for _ in $(seq 1 90); do curl -fsS "$API/v1/health" >/dev/null 2>&1 && break; sleep 1; done
curl -fsS "$API/v1/health"; echo

echo "== create an API key"
KEY=$($DC exec -T gpuctl gpuctl apikey create --name e2e 2>/dev/null)
case $KEY in gpuai_live_*) ;; *) echo "unexpected apikey output"; exit 1;; esac
AUTH="Authorization: Bearer $KEY"
BAD_KEY=gpuai_live_not-a-real-key
code=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $BAD_KEY" "$API/v1/instances")
[ "$code" = 401 ] || { echo "wrong key got HTTP $code, want 401"; exit 1; }

echo "== wait for the catalog"
n=0
for _ in $(seq 1 45); do
  n=$(curl -fsS -H "$AUTH" "$API/v1/pricing" | json 'len(d["data"])')
  [ "$n" -gt 0 ] && break; sleep 2
done
[ "$n" -gt 0 ] || { echo "catalog stayed empty"; exit 1; }
curl -fsS -H "$AUTH" "$API/v1/pricing" | json '"\n".join("  %s x%d %s $%.2f/hr" % (o["gpu_type"], o["gpu_count"], o["region"], o["price_per_hour"]) for o in d["data"])'

echo "== register an SSH key"
ssh-keygen -q -t ed25519 -N '' -C e2e -f "$WORK/key"
curl -fsS -H "$AUTH" -H 'Content-Type: application/json' \
  -d "{\"name\":\"e2e\",\"public_key\":\"$(cat "$WORK/key.pub")\"}" "$API/v1/ssh-keys" | json '"  " + d["id"]'

echo "== POST /v1/instances"
OP=$(curl -fsS -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"gpu_type":"rtx_4090","gpu_count":1,"tier":"on_demand","name":"e2e"}' "$API/v1/instances" | json 'd["operation_id"]')
st=pending
for _ in $(seq 1 60); do
  st=$(curl -fsS -H "$AUTH" "$API/v1/operations/$OP" | json 'd["state"] + " " + (d.get("resource_id") or "")')
  case $st in succeeded*|failed*) break;; esac; sleep 1
done
echo "  operation: $st"
case $st in succeeded*) ;; *) exit 1;; esac
ID=${st#succeeded }
s=unknown
for _ in $(seq 1 90); do
  s=$(curl -fsS -H "$AUTH" "$API/v1/instances/$ID" | json 'd["status"]')
  [ "$s" = running ] && break; sleep 2
done
echo "  instance $ID: $s"
[ "$s" = running ] || exit 1

read -r HOST PORT < <(curl -fsS -H "$AUTH" "$API/v1/instances/$ID" | json 'd["connection"]["hostname"] + " " + str(d["connection"]["port"])')
echo "== ssh root@$HOST -p $PORT"
ssh -i "$WORK/key" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR \
  -o ConnectTimeout=10 -p "$PORT" "root@$HOST" 'echo "  ssh ok on $(hostname)"'

echo "== supplier identity stays out of the API"
if curl -fsS -H "$AUTH" "$API/v1/instances?status=all" | grep -qiE '"(mock|generic)'; then
  echo "  supplier identity leaked into /v1/instances"; exit 1
fi

echo "== DELETE /v1/instances/$ID"
DOP=$(curl -fsS -X DELETE -H "$AUTH" "$API/v1/instances/$ID" | json 'd["operation_id"]')
for _ in $(seq 1 60); do
  st=$(curl -fsS -H "$AUTH" "$API/v1/operations/$DOP" | json 'd["state"]')
  case $st in succeeded|failed) break;; esac; sleep 1
done
echo "  delete operation: $st"
[ "$st" = succeeded ] || exit 1
s=$(curl -fsS -H "$AUTH" "$API/v1/instances/$ID" | json 'd["status"]')
echo "  instance status: $s"
[ "$s" = terminated ] || exit 1

echo "== mock supplier view"
up=$(curl -fsS "$MOCK/mock/instances" | json '" ".join(i["status"] for i in d["instances"] if i["instance_id"] == "'"$ID"'")')
echo "  upstream status: $up"
[ "$up" = terminated ] || exit 1
left=$(docker ps -aq --filter label=io.gpuai-oss.mock-supplier | wc -l | tr -d ' ')
echo "  instance containers left: $left"
[ "$left" = 0 ] || exit 1
echo "E2E PASS"
