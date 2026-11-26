#!/usr/bin/env bash
# Local test of the reference reverse proxy (issue 87). Brings up postgres,
# the app and the Caddy proxy in an isolated Compose project with throwaway
# secrets, checks the proxy's behaviour with curl, then removes the project
# (containers, networks, volumes) and the image it built.
#
#   deploy/proxy/test-proxy.sh
#
# Needs Docker with Compose v2.24+ and curl on the host. Nothing here touches
# another Compose project. Variables (optional):
#   LEARNOO_TEST_IMAGE       use this existing app image instead of building
#                            one (it is then not removed)
#   LEARNOO_TEST_HTTP_PORT   host loopback port for HTTP  (default 18080)
#   LEARNOO_TEST_HTTPS_PORT  host loopback port for HTTPS (default 18443)
#   LEARNOO_TEST_KEEP=1      leave the stack running for inspection
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
deploy=$(dirname "$here")

project="learnoo-proxytest-$$"
http_port="${LEARNOO_TEST_HTTP_PORT:-18080}"
https_port="${LEARNOO_TEST_HTTPS_PORT:-18443}"
curl_image="curlimages/curl:8.17.0@sha256:935d9100e9ba842cdb060de42472c7ca90cfe9a7c96e4dacb55e79e560b3ff40"
# Separate subnets from the reference defaults, so a local stack using
# compose.proxy.yaml can run at the same time.
proxy_ip=172.30.187.10
edge_proxy_ip=172.30.188.10
client_a=172.30.188.21
client_b=172.30.188.22

if [ -n "${LEARNOO_TEST_IMAGE:-}" ]; then
	image="$LEARNOO_TEST_IMAGE"
	built=0
else
	image="learnoo:proxytest-$$"
	built=1
fi

work=$(mktemp -d "${TMPDIR:-/tmp}/learnoo-proxytest.XXXXXX")
install -d -m 0700 "$work/secrets"
for f in pg_superuser_password db_owner_password db_runtime_password; do
	openssl rand -hex 32 >"$work/secrets/$f"
	chmod 0444 "$work/secrets/$f"
done

export LEARNOO_COMPOSE_PROJECT="$project"
export LEARNOO_IMAGE="$image"
export LEARNOO_SECRETS_DIR="$work/secrets"
export LEARNOO_PUBLIC_BASE_URL="https://localhost"
export LEARNOO_PROXY_SITE=localhost
export LEARNOO_PROXY_TLS=internal
export LEARNOO_PROXY_HSTS_MAX_AGE=300
export LEARNOO_PROXY_HTTP_PUBLISH="127.0.0.1:$http_port"
export LEARNOO_PROXY_HTTPS_PUBLISH="127.0.0.1:$https_port"
export LEARNOO_PROXY_IPV4="$proxy_ip"
export LEARNOO_FRONTEND_SUBNET=172.30.187.0/24
export LEARNOO_FRONTEND_IP_RANGE=172.30.187.128/25
export LEARNOO_TEST_EDGE_PROXY_IPV4="$edge_proxy_ip"
export LEARNOO_TEST_EDGE_SUBNET=172.30.188.0/24
export LEARNOO_TEST_EDGE_IP_RANGE=172.30.188.128/25

C=(docker compose -p "$project"
	-f "$deploy/compose.production.yaml"
	-f "$deploy/compose.proxy.yaml"
	-f "$here/compose.test.yaml"
	--env-file "$deploy/production.env.example")

cleanup() {
	status=$?
	if [ "${LEARNOO_TEST_KEEP:-}" = 1 ]; then
		echo "kept: ${C[*]}"
		return
	fi
	echo "== teardown"
	"${C[@]}" --profile ops down -v --remove-orphans >/dev/null 2>&1 || true
	if [ "$built" = 1 ]; then
		docker image rm "$image" >/dev/null 2>&1 || true
	fi
	rm -rf "$work"
	exit "$status"
}
trap cleanup EXIT

pass=0
fail=0
ok() {
	pass=$((pass + 1))
	echo "ok   $*"
}
bad() {
	fail=$((fail + 1))
	echo "FAIL $*"
}
check() { # check <description> <command...>
	local what=$1
	shift
	if "$@"; then ok "$what"; else bad "$what"; fi
}

base="https://localhost:$https_port"
# -k: the certificate comes from Caddy's throwaway local CA (tls internal).
# --path-as-is: send /./ and // exactly as written (curl squashes them).
hcurl() { curl -sk --path-as-is --max-time 20 "$@"; }
status_of() { hcurl -o /dev/null -w '%{http_code}' "$@"; }
header_of() { # header_of <name> <curl args...>
	local name=$1
	shift
	hcurl -o /dev/null -D - "$@" | tr -d '\r' | awk -v n="$(printf '%s' "$name" | tr '[:upper:]' '[:lower:]')" \
		'BEGIN{FS=": "} tolower($1)==n {sub(/^[^:]*: /,""); print; exit}'
}

hdr() { # hdr <name> <saved header file>: the header's value
	tr -d '\r' <"$2" | awk -v n="$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')" \
		'BEGIN{FS=": "} tolower($1)==n {sub(/^[^:]*: /,""); print; exit}'
}
# Headers every Caddy-produced response must carry (redirects aside: no HSTS
# over HTTP) and none may leak.
proxy_headers_ok() { # proxy_headers_ok <saved header file>
	test "$(hdr Strict-Transport-Security "$1")" = "max-age=300" &&
		test "$(hdr Cache-Control "$1")" = no-store &&
		test "$(hdr X-Content-Type-Options "$1")" = nosniff &&
		test "$(hdr Content-Security-Policy "$1")" = "default-src 'none'; frame-ancestors 'none'" &&
		test -z "$(hdr Server "$1")" && test -z "$(hdr Via "$1")"
}

echo "== build and start ($project)"
if [ "$built" = 1 ]; then
	"${C[@]}" build app
fi
"${C[@]}" up -d --wait

echo "== topology"
check "app container healthy (internal /readyz check)" \
	test "$(docker inspect -f '{{.State.Health.Status}}' "$("${C[@]}" ps -q app)")" = healthy
check "app has no published port" \
	test -z "$(docker inspect -f '{{range $p, $b := .NetworkSettings.Ports}}{{if $b}}{{$p}}{{end}}{{end}}' "$("${C[@]}" ps -q app)")"
trusted=$(docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$("${C[@]}" ps -q app)" | sed -n 's/^LEARNOO_TRUSTED_PROXIES=//p')
check "app trusts only the proxy ($trusted)" test "$trusted" = "$proxy_ip/32"
check "proxy has its pinned frontend address" \
	test "$(docker inspect -f "{{(index .NetworkSettings.Networks \"${project}_frontend\").IPAddress}}" "$("${C[@]}" ps -q proxy)")" = "$proxy_ip"

proxy_status=$("${C[@]}" exec -T proxy sh -c 'id -u; grep CapEff /proc/1/status' | tr '\t\n' '  ')
# 0x400 = CAP_NET_BIND_SERVICE only, from the image's file capability on
# the caddy binary (see compose.proxy.yaml); nothing else.
check "proxy runs as uid 65532 with only NET_BIND_SERVICE ($proxy_status)" \
	test "$proxy_status" = "65532 CapEff: 0000000000000400 "

echo "== redirect and headers"
code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 20 "http://127.0.0.1:$http_port/login" -H 'Host: localhost')
loc=$(curl -s -o /dev/null -D - --max-time 20 "http://127.0.0.1:$http_port/login?x=1" -H 'Host: localhost' | tr -d '\r' | sed -n 's/^[Ll]ocation: //p')
check "HTTP -> HTTPS redirect ($code, $loc)" test "$code" = 308 -a "$loc" = "https://localhost/login?x=1"
hsts=$(header_of Strict-Transport-Security "$base/login")
check "HSTS on HTTPS ($hsts)" test "$hsts" = "max-age=300"
check "no HSTS over plain HTTP" test -z "$(curl -s -o /dev/null -D - "http://127.0.0.1:$http_port/login" -H 'Host: localhost' | grep -i strict-transport || true)"
check "GET /login 200 via proxy" test "$(status_of "$base/login")" = 200
csp=$(header_of Content-Security-Policy "$base/login")
# The login page's CSP differs from the proxy's fallback, so this shows the
# app's header was kept.
check "app's own CSP passed through" test -n "$csp" -a "$csp" != "default-src 'none'; frame-ancestors 'none'"
check "no Server header" test -z "$(header_of Server "$base/login")"
check "no Via header" test -z "$(header_of Via "$base/login")"
curl -s -o /dev/null -D "$work/redir.h" --max-time 20 "http://127.0.0.1:$http_port/x" -H 'Host: localhost'
check "redirect: no Server, nosniff, no-store" \
	sh -c "test -z \"\$(grep -i '^server:' '$work/redir.h')\" && grep -qi '^x-content-type-options: nosniff' '$work/redir.h' && grep -qi '^cache-control: no-store' '$work/redir.h'"
studio=$(hcurl -o /dev/null -w '%{http_code} %{redirect_url}' "$base/studio")
check "/studio anonymous -> 303 sign-in ($studio)" test "$studio" = "303 $base/login?next=%2Fstudio"
check "/learn anonymous -> 303 /login" test "$(status_of "$base/learn")" = 303

echo "== probes blocked from outside"
for p in /healthz /readyz /HEALTHZ /%68ealthz /readyz/ "/readyz?x=1" //healthz /./readyz; do
	check "GET $p -> 404" test "$(status_of "$base$p")" = 404
done
check "HEAD /readyz -> 404" test "$(status_of -I "$base/readyz")" = 404
check "probe 404 body is not the app's" test -z "$(hcurl "$base/healthz")"
hcurl -o /dev/null -D "$work/probe.h" "$base/readyz"
check "probe 404 carries HSTS, no-store, nosniff, CSP; no Server or Via" proxy_headers_ok "$work/probe.h"

echo "== request body limit"
head -c $((7 * 1024 * 1024)) /dev/zero >"$work/big"
head -c $((5 * 1024 * 1024 + 1000)) /dev/zero >"$work/ok"
big=$(status_of -D "$work/413.h" -X POST --data-binary @"$work/big" -H 'Content-Type: image/png' "$base/api/media")
check "7 MiB body refused by the proxy ($big)" test "$big" = 413
check "413 carries HSTS, no-store, nosniff, CSP; no Server or Via" proxy_headers_ok "$work/413.h"
small=$(status_of -X POST --data-binary @"$work/ok" -H 'Content-Type: image/png' "$base/api/media")
check "5 MiB body reaches the app (anonymous -> $small)" test "$small" = 401

echo "== authenticated pages are not cached"
printf '%s\n' 'alice-proxytest-password-1' | "${C[@]}" run --rm -T app create-user -email alice@example.test -name AliceProxy >/dev/null
printf '%s\n' 'bob-proxytest-password-22' | "${C[@]}" run --rm -T app create-user -email bob@example.test -name BobProxy >/dev/null
login() { # login <jar> <email> <password>
	hcurl -o /dev/null -w '%{http_code}' -c "$1" -H 'Content-Type: application/json' \
		--data "{\"email\":\"$2\",\"password\":\"$3\"}" "$base/api/auth/login"
}
check "alice signs in" test "$(login "$work/alice" alice@example.test alice-proxytest-password-1)" = 200
check "bob signs in" test "$(login "$work/bob" bob@example.test bob-proxytest-password-22)" = 200
for round in 1 2; do
	for who in alice bob; do
		hcurl -D "$work/$who.h" -o "$work/$who.b" -b "$work/$who" "$base/learn"
		cc=$(tr -d '\r' <"$work/$who.h" | sed -n 's/^[Cc]ache-[Cc]ontrol: //p')
		check "round $round: $who /learn Cache-Control '$cc'" test "$cc" = "private, no-store"
		check "round $round: $who /learn has Vary: Cookie" grep -qi '^vary: .*cookie' "$work/$who.h"
	done
	check "round $round: alice sees her page only" sh -c "grep -q AliceProxy '$work/alice.b' && ! grep -q BobProxy '$work/alice.b'"
	check "round $round: bob sees his page only" sh -c "grep -q BobProxy '$work/bob.b' && ! grep -q AliceProxy '$work/bob.b'"
	hcurl -D "$work/anon.h" -o "$work/anon.b" "$base/learn"
	check "round $round: signed-out /learn gets no user content" \
		sh -c "! grep -q -e AliceProxy -e BobProxy '$work/anon.b' && grep -q '^HTTP/[0-9.]* 303' '$work/anon.h'"
done
check "no Age or cache-status header (proxy is not a cache)" \
	sh -c "! grep -qi -e '^age:' -e '^cache-status:' -e '^x-cache' '$work/alice.h' '$work/bob.h'"

echo "== throttle keyed on the real client"
# Client A: 31 failed sign-ins, each with a different email and a different
# forged X-Forwarded-For. If the forged header were believed, every attempt
# would have its own key and none would be throttled (address limit: 30).
client() { # client <ip> <script>
	docker run --rm --network "${project}_edge" --ip "$1" \
		"$curl_image" sh -c "$2"
}
attempt="curl -sk --resolve localhost:8443:$edge_proxy_ip -o /dev/null -w '%{http_code}\\n' -H 'Content-Type: application/json'"
codes_a=$(client "$client_a" "i=1; while [ \$i -le 31 ]; do $attempt -H \"X-Forwarded-For: 198.51.100.\$i\" --data \"{\\\"email\\\":\\\"nobody\$i@example.test\\\",\\\"password\\\":\\\"wrong-password\\\"}\" https://localhost:8443/api/auth/login; i=\$((i+1)); done")
first30=$(printf '%s\n' "$codes_a" | head -n 30 | sort -u | tr '\n' ' ')
last=$(printf '%s\n' "$codes_a" | tail -n 1)
check "client A: 30 attempts answered 401 ($first30)" test "$first30" = "401 "
check "client A: 31st attempt throttled despite forged headers ($last)" test "$last" = 429
codes_b=$(client "$client_b" "$attempt --data '{\"email\":\"other@example.test\",\"password\":\"wrong-password\"}' https://localhost:8443/api/auth/login; $attempt -H 'X-Forwarded-For: $client_a' --data '{\"email\":\"other2@example.test\",\"password\":\"wrong-password\"}' https://localhost:8443/api/auth/login")
check "client B is not throttled by A ($(echo "$codes_b" | tr '\n' ' '))" test "$(printf '%s\n' "$codes_b" | sort -u)" = 401
check "client A still throttled" test "$(client "$client_a" "$attempt --data '{\"email\":\"x@example.test\",\"password\":\"y\"}' https://localhost:8443/api/auth/login")" = 429

echo "== public and private assets"
asset=$(hcurl "$base/login" | grep -o '/assets/[0-9a-f]\{16\}/[^"]*' | head -n 1)
check "login page names a fingerprinted asset ($asset)" test -n "$asset"
hcurl -o /dev/null -D "$work/asset.h" -b "$work/alice" "$base$asset"
check "/assets/ passes through as public immutable ($(hdr Cache-Control "$work/asset.h"))" \
	test "$(hdr Cache-Control "$work/asset.h")" = "public, max-age=31536000, immutable"
check "/assets/ with a session cookie: no Set-Cookie, no Vary" \
	sh -c "! grep -qi -e '^set-cookie:' -e '^vary:' '$work/asset.h'"
check "/assets/ keeps HSTS" test "$(hdr Strict-Transport-Security "$work/asset.h")" = "max-age=300"
check "/static/site.css stays private, no-store" test "$(header_of Cache-Control "$base/static/site.css")" = "private, no-store"
printf '%s\n' 'admin-proxytest-password-333' | "${C[@]}" run --rm -T app bootstrap-admin -email admin@example.test -name AdminProxy >/dev/null
check "admin signs in" test "$(login "$work/admin" admin@example.test admin-proxytest-password-333)" = 200
# A streamed body (HTTP/2, no Content-Length) over the limit: the app stops
# reading at its own 5 MiB limit and answers 413 before the proxy's 6 MiB
# limit is reached; the proxy's headers are still applied.
csrf=$(hcurl -b "$work/admin" "$base/api/auth/session" | grep -o '"csrfToken":"[^"]*"' | cut -d '"' -f 4)
streamed=$(hcurl -o /dev/null -w '%{http_code} %{http_version}' -D "$work/stream.h" -b "$work/admin" \
	-H "X-CSRF-Token: $csrf" -H 'Content-Type: image/png' -X POST -T - "$base/api/media" <"$work/big")
check "streamed 7 MiB upload without Content-Length refused ($streamed)" test "$streamed" = "413 2"
check "streamed 413: HSTS kept, no Server or Via" \
	sh -c "test '$(hdr Strict-Transport-Security "$work/stream.h")' = max-age=300 && ! grep -qi -e '^server:' -e '^via:' '$work/stream.h'"
sasset=$(hcurl -b "$work/admin" "$base/studio" | grep -o '/studio/assets/[^"]*' | head -n 1)
check "Studio shell names a bundle file ($sasset)" test -n "$sasset"
check "/studio/assets/ for staff: private immutable" \
	test "$(header_of Cache-Control -b "$work/admin" "$base$sasset")" = "private, max-age=31536000, immutable"
anon_sasset=$(hcurl -o /dev/null -w '%{http_code}' -D "$work/sasset.h" "$base$sasset")
check "/studio/assets/ signed out: not served, no-store ($anon_sasset)" \
	sh -c "test '$anon_sasset' != 200 && grep -qi '^cache-control: private, no-store' '$work/sasset.h'"

echo "== access log redaction"
link=$("${C[@]}" run --rm app invite-student -email carol@example.test | grep -o 'https://localhost/invite/[^[:space:]]*')
token=${link##*/}
check "invitation link printed" test -n "$token"
check "invitation page 200 via proxy" test "$(status_of "$base/invite/$token")" = 200
# Non-canonical spellings that still reach the invitation route or name it.
for p in "//invite/$token" "/./invite/$token" "/%69nvite/$token" "/Invite/$token" "/invite/$token?x=1"; do
	echo "     ${p//$token/<token>} -> $(status_of "$base$p")"
done
status_of "$base/learn/search?q=query-marker-$$" -b "$work/alice" >/dev/null
alice_cookie=$(awk '$6 ~ /learnoo_session/ {print $7}' "$work/alice")
# The app is stopped so the next request fails at the proxy (502), which
# goes through Caddy's error log as well as the access log.
"${C[@]}" stop app >/dev/null 2>&1
check "invitation path with app down -> 502" test "$(status_of "$base/invite/$token")" = 502
check "encoded invitation path with app down -> 502" test "$(status_of -D "$work/502.h" "$base/%69nvite/$token")" = 502
check "502 carries HSTS, no-store, nosniff, CSP; no Server or Via" proxy_headers_ok "$work/502.h"
sleep 1
"${C[@]}" logs --no-log-prefix proxy >"$work/proxy.log" 2>&1
check "access log is JSON on stdout" grep -q '"logger":"http.log.access' "$work/proxy.log"
check "redacted invitation path logged" grep -q '"uri":"/invite/REDACTED"' "$work/proxy.log"
echo "     sample: $(grep -m1 '"uri":"/invite/REDACTED"' "$work/proxy.log" | cut -c1-400)"
check "invitation token absent from proxy logs" sh -c "! grep -qF '$token' '$work/proxy.log'"
check "query string absent from proxy logs" sh -c "! grep -qF 'query-marker-$$' '$work/proxy.log'"
check "session cookie absent from proxy logs" sh -c "test -n '$alice_cookie' && ! grep -qF '$alice_cookie' '$work/proxy.log'"
check "no Cookie or Set-Cookie fields in proxy logs" sh -c "! grep -qi -e '\"Cookie\":' -e '\"Set-Cookie\":' '$work/proxy.log'"

echo "== $pass passed, $fail failed"
test "$fail" = 0
