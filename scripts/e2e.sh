#!/usr/bin/env bash
# Runs the tests against real services under e2e/: a real PostgreSQL, MySQL, Redis, ClickHouse and
# Kafka, with real processes and real signals.
#
#   scripts/e2e.sh                 # everything (the load tests and the slow ones aside)
#   scripts/e2e.sh -run Smoke      # the arguments after it go to go test as they are
#   scripts/e2e.sh --slow          # the slow cases as well (XONE_E2E_SLOW=1), see requireSlow
#   scripts/e2e.sh --load -run Load  # the load tests as well (XONE_E2E_LOAD=1); one load round takes
#                                    # a dozen minutes
#
# A load test needs the machine to itself: the load generator, the service under test and PG already
# share one machine, and any further load makes the numbers meaningless.
#
# The services may come from any of three places:
#   - installed on this host: one already running is used, and one that is not is brought up the way
#     this host installs it (pg_ctlcluster / service mysql / redis-server);
#   - docker compose: docker compose -f e2e/compose.yml up -d --wait, whose accounts and passwords
#     match the defaults here;
#   - taken care of elsewhere (CI's service containers, another machine): XONE_E2E_EXTERNAL=1, or an
#     address that is not on this host -- it is then only waited for (60 seconds at most) and never
#     started.
# Readiness goes by whether TCP connects (through bash's /dev/tcp) and requires no psql / mysqladmin
# / redis-cli; it is wrapped in timeout (or gtimeout, or nothing at all on a host that has neither,
# see tcp_up); with a client installed it connects once more with the test's account, so a wrong
# account or database is reported once here rather than once per case.
#
# ClickHouse is optional: one that is running is used, and on this host it runs in the Docker
# container xone-ch (XONE_E2E_CH_CONTAINER), which is docker started when stopped. No Docker, no such
# container, or one that fails to come up is not a failure -- the CH cases skip themselves with a
# reason and the rest run as usual. XONE_E2E_CH=0 set beforehand leaves CH alone.
# Kafka is optional in the same way: one that is running is used, and on this host it runs in the
# Docker container xone-kafka (XONE_E2E_KAFKA_CONTAINER), which is docker started when stopped and
# docker run from e2e/compose.yml's settings when there is no such container (a single KRaft node
# with its data in tmpfs).
# No Docker, or one that fails to come up, is not a failure -- the Kafka cases skip themselves.
# XONE_E2E_KAFKA=0 set beforehand leaves Kafka alone.
# Every connection setting can be overridden through the environment, with the defaults matching
# e2e/harness's.
#
# Not part of scripts/test.sh: there is no database there, and every e2e test skips itself while
# XONE_E2E is not 1.
# In CI it is a workflow of its own (.github/workflows/e2e.yml), running only on a PR that changed a
# go.mod / go.sum, nightly, and by hand.
set -e
cd "$(dirname "$0")/.."

# --load may be written anywhere, and the other arguments are left to go test as they are
#
# Without a -parallel it is 16: go test runs only GOMAXPROCS parallel tests at a time by default (4
# on a 4-core machine), while an e2e test spends most of its time waiting -- for a timeout, for an
# exit after a signal, for a database -- with the CPU idle and the tests queueing.
# Measured on a 4-core machine with the databases ready: 4 -> 103s, 16 -> 60s (two rounds green);
# 32 is no faster (held at 60s by the slowest test) and makes a timing assertion such as "exits
# within 1s" time out while fighting for the CPU (4 red)
timeout=30m
parallel=-parallel=16
for a; do
  shift
  case "$a" in
  --load)
    export XONE_E2E_LOAD=1
    timeout=60m
    continue
    ;;
  --slow)
    export XONE_E2E_SLOW=1
    continue
    ;;
  -parallel|-parallel=*|-test.parallel|-test.parallel=*) parallel= ;;
  esac
  set -- "$@" "$a"
done

: "${XONE_E2E_PG_ADDR:=127.0.0.1:5432}"
: "${XONE_E2E_PG_USER:=xone}"
: "${XONE_E2E_PG_PASSWORD:=e2e-secret-pw}"
: "${XONE_E2E_PG_DB:=xone_e2e}"
: "${XONE_E2E_REDIS_ADDR:=127.0.0.1:6379}"
: "${XONE_E2E_MYSQL_ADDR:=127.0.0.1:3306}"
: "${XONE_E2E_MYSQL_USER:=xone}"
: "${XONE_E2E_MYSQL_PASSWORD:=e2e-secret-pw}"
: "${XONE_E2E_MYSQL_DB:=xone_e2e}"
# pg_ctlcluster's version and cluster name, deliberately unquoted: it is two arguments
: "${XONE_E2E_PG_CLUSTER:=16 main}"
export XONE_E2E_PG_ADDR XONE_E2E_PG_USER XONE_E2E_PG_PASSWORD XONE_E2E_PG_DB XONE_E2E_REDIS_ADDR
export XONE_E2E_MYSQL_ADDR XONE_E2E_MYSQL_USER XONE_E2E_MYSQL_PASSWORD XONE_E2E_MYSQL_DB
: "${XONE_E2E_CH_ADDR:=127.0.0.1:9000}"
: "${XONE_E2E_CH_HTTP_ADDR:=127.0.0.1:8123}"
: "${XONE_E2E_CH_USER:=xone}"
: "${XONE_E2E_CH_PASSWORD:=e2e-secret-pw}"
: "${XONE_E2E_CH_DB:=xone_e2e}"
: "${XONE_E2E_CH_CONTAINER:=xone-ch}"
export XONE_E2E_CH_ADDR XONE_E2E_CH_HTTP_ADDR XONE_E2E_CH_USER XONE_E2E_CH_PASSWORD XONE_E2E_CH_DB
: "${XONE_E2E_KAFKA_ADDR:=127.0.0.1:9092}"
: "${XONE_E2E_KAFKA_CONTAINER:=xone-kafka}"
: "${XONE_E2E_KAFKA_IMAGE:=mirror.gcr.io/apache/kafka:3.9.1}"
export XONE_E2E_KAFKA_ADDR

# The address is host:port; taking the host removes IPv6's square brackets
host_of() { h=${1%:*}; h=${h#[}; echo "${h%]}"; }
is_local() { case $(host_of "$1") in 127.*|localhost|::1) ;; *) return 1;; esac; }
has() { command -v "$1" >/dev/null 2>&1; }

# timeout is GNU coreutils' and macOS does not ship it; Homebrew installs it as gtimeout. Without
# either, tcp_up below runs the connect without one: /dev/tcp then waits out the OS's own connect
# timeout, which only ever shows up for an address that drops packets -- a refused or an accepted
# connection answers immediately.
#
# Calling a missing timeout was the worse half of this: tcp_up redirects stderr to /dev/null, so
# "command not found" was swallowed, every service was reported as never connecting, and e2e could
# not be run on a Mac at all.
if has timeout; then TIMEOUT=timeout
elif has gtimeout; then TIMEOUT=gtimeout
else TIMEOUT=""
fi

# tcp_up <address>: connecting counts as up. It uses bash's own /dev/tcp alone, and the timeout
# guards against hanging on an address that is a black hole
tcp_up() { ${TIMEOUT:+$TIMEOUT 2} bash -c ': <>"/dev/tcp/$0/$1"' "$(host_of "$1")" "${1##*:}" 2>/dev/null; }

# wait_for <attempts> <command>...: until the command succeeds, trying every 0.2 seconds
wait_for() {
  tries=$1
  shift
  i=0
  until "$@" >/dev/null 2>&1; do
    i=$((i + 1))
    [ "$i" -lt "$tries" ] || return 1
    sleep 0.2
  done
}

# ensure <name> <address> <start command>...: one that is running is used; one taken care of
# elsewhere, or with no such start command on this host, is only waited for; otherwise it is brought
# up
ensure() {
  what=$1 addr=$2
  shift 2
  if tcp_up "$addr"; then
    echo "✓ $what is already running on $addr"
    return
  fi
  if [ "${XONE_E2E_EXTERNAL:-}" = 1 ] || ! is_local "$addr" || ! has "$1"; then
    echo "== waiting for $what to be ready on $addr"
    wait_for 300 tcp_up "$addr" || {
      echo "✗ $what did not connect on $addr within 60 seconds. Without it installed on this host: docker compose -f e2e/compose.yml up -d --wait"
      exit 1
    }
  else
    echo "== starting ${what} ($*)"
    "$@" >/dev/null
    wait_for 50 tcp_up "$addr" || { echo "✗ $what did not come up"; exit 1; }
  fi
  echo "✓ $what is ready"
}

# ---- PostgreSQL ----
# shellcheck disable=SC2086  # $XONE_E2E_PG_CLUSTER is two arguments
ensure PostgreSQL "$XONE_E2E_PG_ADDR" pg_ctlcluster $XONE_E2E_PG_CLUSTER start
if has psql && ! PGPASSWORD=$XONE_E2E_PG_PASSWORD psql -h "$(host_of "$XONE_E2E_PG_ADDR")" -p "${XONE_E2E_PG_ADDR##*:}" \
  -U "$XONE_E2E_PG_USER" -d "$XONE_E2E_PG_DB" -tAc 'SELECT 1' >/dev/null 2>&1; then
  cat <<TIP
✗ $XONE_E2E_PG_USER cannot connect to the database ${XONE_E2E_PG_DB} on $XONE_E2E_PG_ADDR. On a first run, create the account and the database first:

  su postgres -c "psql -c \"CREATE ROLE $XONE_E2E_PG_USER LOGIN PASSWORD '<password>'\""
  su postgres -c "createdb -O $XONE_E2E_PG_USER $XONE_E2E_PG_DB"

Tell this script the password through XONE_E2E_PG_PASSWORD.
TIP
  exit 1
fi

# ---- MySQL ----
ensure MySQL "$XONE_E2E_MYSQL_ADDR" service mysql start
# The password reaches the client through MYSQL_PWD and never appears on the command line (which ps
# shows)
if has mysql && ! MYSQL_PWD=$XONE_E2E_MYSQL_PASSWORD mysql -h "$(host_of "$XONE_E2E_MYSQL_ADDR")" -P "${XONE_E2E_MYSQL_ADDR##*:}" \
  -u "$XONE_E2E_MYSQL_USER" -D "$XONE_E2E_MYSQL_DB" -Nse 'SELECT 1' >/dev/null 2>&1; then
  cat <<TIP
✗ $XONE_E2E_MYSQL_USER cannot connect to the database ${XONE_E2E_MYSQL_DB} on $XONE_E2E_MYSQL_ADDR. On a first run, create the account and the database first:

  mysql -uroot -e "CREATE DATABASE $XONE_E2E_MYSQL_DB; CREATE USER '$XONE_E2E_MYSQL_USER'@'127.0.0.1' IDENTIFIED BY '<password>';
    GRANT ALL ON $XONE_E2E_MYSQL_DB.* TO '$XONE_E2E_MYSQL_USER'@'127.0.0.1'"

Tell this script the password through XONE_E2E_MYSQL_PASSWORD.
TIP
  exit 1
fi

# ---- Redis ----
# Nothing lands on disk: the script's working directory right now is the repository root, which is
# Redis's default dir. With the default save rules (a BGSAVE after 100 changes within 300 seconds),
# one e2e round would leave a dump.rdb in the repository root
ensure Redis "$XONE_E2E_REDIS_ADDR" redis-server --port "${XONE_E2E_REDIS_ADDR##*:}" --daemonize yes --save '' --appendonly no

# ---- ClickHouse ----
# One that fails to come up sets XONE_E2E_CH=0: harness.RequireCH skips the CH cases because of it
# and says so in the skip reason.
# Without curl it falls back to whether the native port connects
ch_ping() {
  if has curl; then curl -sf "http://$XONE_E2E_CH_HTTP_ADDR/ping" >/dev/null 2>&1; else tcp_up "$XONE_E2E_CH_ADDR"; fi
}
# The credentials reach curl -K - through standard input and never appear on the command line;
# without curl nothing is checked and the cases report it
ch_auth() {
  has curl || return 0
  printf 'user = "%s:%s"\n' "$XONE_E2E_CH_USER" "$XONE_E2E_CH_PASSWORD" |
    curl -sf -K - "http://$XONE_E2E_CH_HTTP_ADDR/?database=$XONE_E2E_CH_DB" --data-binary 'SELECT 1' >/dev/null 2>&1
}
ch_skip() {
  echo "⚠ ClickHouse is unavailable ($1) and the CH cases will skip; to run them: docker start ${XONE_E2E_CH_CONTAINER}, or docker compose -f e2e/compose.yml up -d --wait"
  export XONE_E2E_CH=0
}
if [ "${XONE_E2E_CH:-}" = 0 ]; then
  echo "⚠ XONE_E2E_CH=0 and the CH cases will skip"
elif ch_ping; then
  echo "✓ ClickHouse is already running on $XONE_E2E_CH_ADDR"
elif [ "${XONE_E2E_EXTERNAL:-}" = 1 ] || ! is_local "$XONE_E2E_CH_ADDR"; then
  echo "== waiting for ClickHouse to be ready on $XONE_E2E_CH_ADDR"
  if wait_for 300 ch_ping; then echo "✓ ClickHouse is ready"; else ch_skip "it did not connect on $XONE_E2E_CH_ADDR within 60 seconds"; fi
elif ! has docker; then
  ch_skip "there is no docker command"
elif ! docker inspect "$XONE_E2E_CH_CONTAINER" >/dev/null 2>&1; then
  ch_skip "there is no container named $XONE_E2E_CH_CONTAINER"
else
  echo "== starting ClickHouse (docker start ${XONE_E2E_CH_CONTAINER})"
  if docker start "$XONE_E2E_CH_CONTAINER" >/dev/null 2>&1; then
    # ClickHouse starts cold more slowly than PG, so it gets 30 seconds
    wait_for 150 ch_ping || true
  fi
  if ch_ping; then echo "✓ ClickHouse has started"; else ch_skip "it was still not ready 30 seconds after docker start; see docker logs $XONE_E2E_CH_CONTAINER"; fi
fi
if [ "${XONE_E2E_CH:-}" != 0 ] && ! ch_auth; then
  ch_skip "$XONE_E2E_CH_USER cannot connect to the database ${XONE_E2E_CH_DB} on $XONE_E2E_CH_HTTP_ADDR; tell this script the password through XONE_E2E_CH_PASSWORD"
fi

# ---- Kafka ----
# Readiness cannot go by the port alone: docker's port forwarding listens before the broker is up.
# The container holds Kafka's own scripts, which ask the broker for its ApiVersions once; one not
# taken care of by a local container (CI's service containers, another machine) falls back to the
# port alone, and harness.RequireKafka checks the real readiness once more.
# A cold start was measured at about 7s (the JVM included), and the first docker run also pulls the
# image (about 200MB)
kafka_container_up() {
  docker exec "$XONE_E2E_KAFKA_CONTAINER" /opt/kafka/bin/kafka-broker-api-versions.sh \
    --bootstrap-server "127.0.0.1:${XONE_E2E_KAFKA_ADDR##*:}" >/dev/null 2>&1
}
kafka_skip() {
  echo "⚠ Kafka is unavailable ($1) and the Kafka cases will skip; to run them: docker compose -f e2e/compose.yml up -d --wait kafka"
  export XONE_E2E_KAFKA=0
}
# kafka_run starts a container from e2e/compose.yml's settings: a single KRaft node, with the first
# assignment of a consumer group not waiting 3s (GROUP_INITIAL_REBALANCE_DELAY_MS defaults to 3000
# and every case joins a group, so what is saved is the time of the whole e2e round)
kafka_run() {
  port=${XONE_E2E_KAFKA_ADDR##*:}
  docker run -d --name "$XONE_E2E_KAFKA_CONTAINER" -p "127.0.0.1:$port:$port" --tmpfs /tmp/kraft-combined-logs \
    -e KAFKA_NODE_ID=1 -e KAFKA_PROCESS_ROLES=broker,controller \
    -e KAFKA_LISTENERS="PLAINTEXT://:$port,CONTROLLER://:9093" \
    -e KAFKA_ADVERTISED_LISTENERS="PLAINTEXT://127.0.0.1:$port" \
    -e KAFKA_CONTROLLER_LISTENER_NAMES=CONTROLLER \
    -e KAFKA_LISTENER_SECURITY_PROTOCOL_MAP=CONTROLLER:PLAINTEXT,PLAINTEXT:PLAINTEXT \
    -e KAFKA_CONTROLLER_QUORUM_VOTERS=1@localhost:9093 \
    -e KAFKA_OFFSETS_TOPIC_REPLICATION_FACTOR=1 -e KAFKA_TRANSACTION_STATE_LOG_REPLICATION_FACTOR=1 \
    -e KAFKA_TRANSACTION_STATE_LOG_MIN_ISR=1 -e KAFKA_GROUP_INITIAL_REBALANCE_DELAY_MS=0 \
    "$XONE_E2E_KAFKA_IMAGE" >/dev/null
}
if [ "${XONE_E2E_KAFKA:-}" = 0 ]; then
  echo "⚠ XONE_E2E_KAFKA=0 and the Kafka cases will skip"
elif [ "${XONE_E2E_EXTERNAL:-}" = 1 ] || ! is_local "$XONE_E2E_KAFKA_ADDR"; then
  echo "== waiting for Kafka to be ready on $XONE_E2E_KAFKA_ADDR"
  if wait_for 300 tcp_up "$XONE_E2E_KAFKA_ADDR"; then echo "✓ Kafka is ready"; else kafka_skip "it did not connect on $XONE_E2E_KAFKA_ADDR within 60 seconds"; fi
elif ! has docker || ! docker info >/dev/null 2>&1; then
  if tcp_up "$XONE_E2E_KAFKA_ADDR"; then echo "✓ Kafka is already running on $XONE_E2E_KAFKA_ADDR"; else kafka_skip "there is no docker, or the docker daemon is not running"; fi
elif kafka_container_up; then
  echo "✓ Kafka is already running on $XONE_E2E_KAFKA_ADDR (the container ${XONE_E2E_KAFKA_CONTAINER})"
else
  kafka_start=$(date +%s)
  if docker inspect "$XONE_E2E_KAFKA_CONTAINER" >/dev/null 2>&1; then
    echo "== starting Kafka (docker start ${XONE_E2E_KAFKA_CONTAINER})"
    docker start "$XONE_E2E_KAFKA_CONTAINER" >/dev/null 2>&1 || true
  else
    echo "== creating the Kafka container (docker run ${XONE_E2E_KAFKA_IMAGE}, named ${XONE_E2E_KAFKA_CONTAINER})"
    kafka_run || true
  fi
  # Every probe starts a JVM of its own (about 2s), so the count is kept low: about 60 seconds in
  # total
  if wait_for 30 kafka_container_up; then
    echo "✓ Kafka has started ($(($(date +%s) - kafka_start))s)"
  else
    kafka_skip "it was still not ready 60 seconds after starting; see docker logs $XONE_E2E_KAFKA_CONTAINER"
  fi
fi

# GOWORK=off as in scripts/test.sh: what is tested are the dependencies e2e/go.mod resolves on its
# own
cd e2e
export XONE_E2E=1 GOWORK=off
exec go test -count=1 -v -timeout "$timeout" $parallel ./... "$@"
