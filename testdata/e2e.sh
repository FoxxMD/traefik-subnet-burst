#!/usr/bin/env bash
set -u
fail=0
for _ in $(seq 1 30); do curl -s -o /dev/null localhost:18080/ && break; sleep 1; done
check() { # want ip path
  got=$(curl -s -o /dev/null -w '%{http_code}' -H "X-Test-Client-Ip: $2" "http://localhost:18080$3")
  if [ "$got" = "$1" ]; then echo "ok   $got $2 $3"; else echo "FAIL got $got want $1: $2 $3"; fail=1; fi
}
check 200 195.64.119.47  /api/script.js
check 200 195.64.115.161 /api/script.js
check 200 5.183.91.175   /api/script.js
check 403 195.64.3.3     /api/script.js
check 403 195.64.200.1   /api/track
check 200 5.183.91.175   /api/track
exit $fail
