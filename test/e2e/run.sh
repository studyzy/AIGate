#!/usr/bin/env bash
# Tencent Cloud AIGate end-to-end smoke test entry point. It owns the whole lifecycle of the test
# environment, so every run starts from a clean state and leaves nothing behind:
#
#   SQLite: delete and recreate the database file -> wait until it is writable ->
#           build the binaries -> run test/e2e -> delete the database file.
#   MySQL:  start a fresh MySQL container -> wait until it answers a query -> reset the schema ->
#           build the binaries -> run test/e2e -> stop and remove the container. Only `docker` plus
#           the MySQL image are needed; the image's own mysql client inside the container creates
#           the schema, so nothing is installed on the host.
#           With AIGATE_E2E_MYSQL_USE_EXISTING=1 the script instead talks to an already running
#           MySQL service over TCP (host mysql client, no docker): CI declares one as a job service,
#           whose container name is chosen by the runner and therefore cannot be addressed by name.
#
# The gateway processes started by the test cases are stopped in their own t.Cleanup; the exit trap
# here is a backstop that kills anything still running against the test database (it never touches
# an unrelated gateway such as the local systemd service).
#
# Usage (works from any directory in the repository):
#   bash test/e2e/run.sh                          # default: MySQL
#   AIGATE_E2E_DB_TYPE=sqlite bash test/e2e/run.sh
#
# Environment variables can override the defaults:
#   AIGATE_E2E_DB_TYPE (default mysql | sqlite)
#   MYSQL_HOST (default 127.0.0.1), MYSQL_PORT (default 3306), MYSQL_USER (default root)
#   AIGATE_E2E_MYSQL_IMAGE (default mysql:8.0), AIGATE_E2E_MYSQL_CONTAINER (default aigate-e2e-mysql),
#   AIGATE_E2E_MYSQL_PASSWORD (default AIGate-e2e; the container root password, never empty)
#   AIGATE_E2E_MYSQL_USE_EXISTING (default 0; set 1 to reuse an already running MySQL service via
#                                  TCP instead of starting a container — needs a host `mysql` client,
#                                  e.g. a GitHub Actions services container)
#   AIGATE_E2E_SQLITE_DSN (sqlite only, default ./.e2e/aigate_e2e.db; deleted and recreated on every run)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
cd "${REPO_ROOT}"

DB_TYPE="${AIGATE_E2E_DB_TYPE:-mysql}"

DB_NAME="aigate_e2e"
MYSQL_HOST="${MYSQL_HOST:-127.0.0.1}"
MYSQL_PORT="${MYSQL_PORT:-3306}"
MYSQL_USER="${MYSQL_USER:-root}"
MYSQL_IMAGE="${AIGATE_E2E_MYSQL_IMAGE:-mysql:8.0}"
MYSQL_CONTAINER="${AIGATE_E2E_MYSQL_CONTAINER:-aigate-e2e-mysql}"
# 为 1 时假定容器已在运行（CI services 场景），不自行 docker run，退出时也不 docker rm。
USE_EXISTING_MYSQL="${AIGATE_E2E_MYSQL_USE_EXISTING:-0}"
# 容器 root 密码：不用空密码/弱密码。可用 AIGATE_E2E_MYSQL_PASSWORD 覆盖，避免明文出现在 ps 输出里。
MYSQL_PASSWORD="${AIGATE_E2E_MYSQL_PASSWORD:-AIGate-e2e}"
SQLITE_DSN="${AIGATE_E2E_SQLITE_DSN:-${REPO_ROOT}/.e2e/aigate_e2e.db}"

INIT_SQL="${REPO_ROOT}/sql/init.sql"
AIGATE_BIN="${REPO_ROOT}/bin/aigate"
CLI_BIN="${REPO_ROOT}/bin/aigatectl"

# container_started=1 表示本次运行由脚本自己创建的容器，退出时负责删除。
container_started=0

log() { printf '[e2e] %s\n' "$*"; }

# ---------------------------------------------------------------- 清理

# stop_gateways 停掉仍连着测试库的 AIGate 进程。按子进程环境里的 AIGATE_DB_DSN 精确匹配
# （DSN 在 argv 里但环境更易唯一命中），避免误杀本机常驻的网关服务。
stop_gateways() {
  if [[ -z "${AIGATE_E2E_DSN:-}" ]]; then
    return 0
  fi
  for pid in $(pgrep -f -- "bin/aigate" 2>/dev/null || true); do
    if tr '\0' '\n' <"/proc/${pid}/environ" 2>/dev/null | grep -qxF "AIGATE_DB_DSN=${AIGATE_E2E_DSN}"; then
      kill -TERM "${pid}" 2>/dev/null || true
    fi
  done
}

# remove_sqlite_files 删除 SQLite 库文件及 WAL 附属文件。
remove_sqlite_files() {
  rm -f "${SQLITE_DSN}" "${SQLITE_DSN}-wal" "${SQLITE_DSN}-shm"
}

cleanup() {
  local rc=$?
  set +e

  stop_gateways

  if [[ "${DB_TYPE}" == "sqlite" ]]; then
    remove_sqlite_files
  elif [[ "${container_started}" == "1" ]]; then
    log "Removing MySQL container ${MYSQL_CONTAINER} ..."
    docker rm -f "${MYSQL_CONTAINER}" >/dev/null 2>&1
  fi

  return "${rc}"
}
trap cleanup EXIT

# ---------------------------------------------------------------- 数据库

if [[ "${DB_TYPE}" == "mysql" ]]; then
  if [[ ! -f "${INIT_SQL}" ]]; then
    log "Schema file ${INIT_SQL} not found, cannot initialize the test database; make sure the repository contains sql/init.sql."
    exit 1
  fi

  # 只有「自行起容器」才需要 docker。复用已有服务（CI services / 本机常驻实例）时
  # 走宿主机 TCP 客户端，不依赖 docker，也不假设容器的名字。
  if [[ "${USE_EXISTING_MYSQL}" != "1" ]] && ! command -v docker >/dev/null 2>&1; then
    log "docker not found, a MySQL container (or AIGATE_E2E_MYSQL_CONTAINER) is required to run the MySQL e2e."
    exit 1
  fi

  # 复用已在运行的服务（如 CI 的 services 容器）：只连进去建库导入，退出时不停不删。
  # 这条路径直接用宿主机 mysql 客户端连 TCP —— GitHub Actions 的 service 容器名是
  # `<job>-<service>-<随机后缀>`，与本地约定的名字不同，经 `docker exec <name>` 必然找不到，
  # 历史上正是这样让 e2e (mysql) 卡满 180 秒后失败。
  if [[ "${USE_EXISTING_MYSQL}" == "1" ]]; then
    log "Reusing the MySQL service on ${MYSQL_HOST}:${MYSQL_PORT} ..."
  else
    # 每次都新起容器：先清掉同名旧容器，保证不残留上一次的数据卷/进程。
    if docker ps -a --format '{{.Names}}' | grep -qx "${MYSQL_CONTAINER}"; then
      log "Removing the leftover container ${MYSQL_CONTAINER} ..."
      docker rm -f "${MYSQL_CONTAINER}" >/dev/null
    fi

    log "Starting MySQL container ${MYSQL_CONTAINER} (${MYSQL_IMAGE}) on ${MYSQL_HOST}:${MYSQL_PORT} ..."
    docker run -d --name "${MYSQL_CONTAINER}" \
      -e MYSQL_ROOT_PASSWORD="${MYSQL_PASSWORD}" \
      -p "${MYSQL_PORT}:3306" \
      "${MYSQL_IMAGE}" >/dev/null
    container_started=1
  fi

  # 密码一律经环境变量 MYSQL_PWD 注入客户端进程，不落在命令行参数里。
  # 显式走 -h 而不是默认 unix socket：入口脚本重启 mysqld 的瞬间，上一代的 socket 文件
  # 可能仍在而新进程尚未监听，只测 socket 会假性通过，随后建库就报 2002。
  if [[ "${USE_EXISTING_MYSQL}" == "1" ]]; then
    if ! command -v mysql >/dev/null 2>&1; then
      log "mysql client not found; it is required to reuse an already running MySQL service (AIGATE_E2E_MYSQL_USE_EXISTING=1)."
      log "Install it (e.g. apt-get install -y mysql-client) or unset AIGATE_E2E_MYSQL_USE_EXISTING to let the script start its own container."
      exit 1
    fi
    # 复用已有服务：直连宿主机映射端口，不经过 docker。
    mysql_exec() {
      MYSQL_PWD="${MYSQL_PASSWORD}" mysql \
        -h "${MYSQL_HOST}" -P "${MYSQL_PORT}" -uroot "$@"
    }
  else
    # 自起容器：建库与导入全部用镜像自带的 mysql 客户端经 docker exec 完成，宿主机无需 MySQL 工具。
    mysql_exec() {
      docker exec -i -e "MYSQL_PWD=${MYSQL_PASSWORD}" "${MYSQL_CONTAINER}" \
        mysql -h 127.0.0.1 -uroot "$@"
    }
  fi

  log "Waiting for MySQL to accept queries on ${MYSQL_HOST}:${MYSQL_PORT} ..."
  # 以「连着两次跑得通 SELECT 1」作为就绪判据，而不是只看日志：初始化阶段那个禁网的临时 mysqld
  # 也会打印 "ready for connections"，只看日志会导致误判、提前建库（表现为 Lost connection）。
  deadline=$((SECONDS + 180))
  consecutive=0
  while ((consecutive < 2)); do
    if mysql_exec -e 'SELECT 1' >/dev/null 2>&1; then
      consecutive=$((consecutive + 1))
    else
      consecutive=0
    fi
    if ((consecutive < 2)); then
      if ((SECONDS >= deadline)); then
        log "MySQL did not become ready in time on ${MYSQL_HOST}:${MYSQL_PORT}."
        if [[ "${USE_EXISTING_MYSQL}" != "1" ]]; then
          docker ps -a --filter "name=^${MYSQL_CONTAINER}$"
          docker logs --tail 50 "${MYSQL_CONTAINER}" || true
        else
          log "Check that the service is up and reachable on that host/port."
        fi
        exit 1
      fi
      sleep 2
    fi
  done
  log "MySQL is ready."

  log "Resetting test database ${DB_NAME} ..."
  mysql_exec -e "DROP DATABASE IF EXISTS \`${DB_NAME}\`; CREATE DATABASE \`${DB_NAME}\` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;"

  log "Importing ${INIT_SQL} ..."
  mysql_exec "${DB_NAME}" < "${INIT_SQL}"

  export AIGATE_E2E_DSN="${MYSQL_USER}:${MYSQL_PASSWORD}@tcp(${MYSQL_HOST}:${MYSQL_PORT})/${DB_NAME}?charset=utf8mb4&parseTime=True&loc=Local"
elif [[ "${DB_TYPE}" == "sqlite" ]]; then
  # SQLite 建表由网关启动时的 AutoMigrate 完成，不需要 init.sql；删掉旧库文件即可保证从空库开始。
  mkdir -p "$(dirname "${SQLITE_DSN}")"
  rm -f "${SQLITE_DSN}" "${SQLITE_DSN}-wal" "${SQLITE_DSN}-shm"
  : >"${SQLITE_DSN}"
  [[ -w "${SQLITE_DSN}" ]]
  log "Recreated SQLite test database ${SQLITE_DSN}"

  export AIGATE_E2E_DSN="${SQLITE_DSN}"
else
  log "Unsupported AIGATE_E2E_DB_TYPE=${DB_TYPE} (want mysql or sqlite)."
  exit 1
fi

# ---------------------------------------------------------------- 构建与运行

log "Building gateway binary ${AIGATE_BIN} ..."
mkdir -p "${REPO_ROOT}/bin"
go build -o "${AIGATE_BIN}" ./cmd/aigate

log "Building CLI binary ${CLI_BIN} ..."
go build -o "${CLI_BIN}" ./cmd/aigatectl

export AIGATE_E2E_BIN="${AIGATE_BIN}"
export AIGATE_E2E_CLI_BIN="${CLI_BIN}"
export AIGATE_E2E_DB_TYPE="${DB_TYPE}"
log "AIGATE_E2E_DB_TYPE=${AIGATE_E2E_DB_TYPE} AIGATE_E2E_DSN=${AIGATE_E2E_DSN%%@*}@... (user only, password masked)"

log "Running e2e (go test -tags e2e) ..."
go test -tags e2e -count=1 -v ./test/e2e/