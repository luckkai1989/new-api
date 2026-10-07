#!/usr/bin/env bash
# RayApi deployment: /opt/newapi, Docker Compose, MySQL database newapi.
set -Eeuo pipefail
umask 077

DEPLOY=/opt/newapi
BACKUP_ROOT=/opt/newapi-backups
SOURCE=/root/newapi-update/new-api
CUSTOM_OVERRIDE=$DEPLOY/docker-compose.custom.yml
# A separate image-only layer preserves existing custom environment settings.
IMAGE_OVERRIDE=$DEPLOY/docker-compose.image.yml
CUSTOM_IMAGE=ghcr.io/luckkai1989/new-api
CUSTOM_REPOSITORY=https://github.com/luckkai1989/new-api
BACKUP=''
STOPPED=0
MIGRATION_POSSIBLE=0
MODE=''
SAVE_IMAGES=0
KEEP_BACKUPS=3
IMAGE_OVERRIDE_PRESENT=0
IMAGE_OVERRIDE_CHANGED=0
MONITOR_RECREATE=0
usage() {
  printf 'Usage: bash %s [--upgrade|--upgrade-custom|--backup|--backup-db|--backup-images|--check|--help]\n' "${0##*/}"
  printf 'Compatibility: --upgrade-source now pulls the GitHub-built image; it never builds locally.\n'
}
show_menu() {
  printf '\n========== RayApi / NewAPI 运维菜单 ==========\n'
  printf '  1) 备份并升级官方 NewAPI（短暂停服）\n'
  printf '  2) 备份 NewAPI + MySQL + Redis + 配置（短暂停服，不升级）\n'
  printf '  3) 仅在线备份 MySQL 数据库（不停服）\n'
  printf '  4) 完整备份并导出三个容器的镜像（需要更多磁盘）\n'
  printf '  5) 仅检查部署（不升级、不停服）\n'
  printf '  6) 拉取 main 最新提交的 GitHub 二开镜像，备份后升级（不在服务器编译）\n'
  printf '  0) 退出\n'
}
[[ $# -le 1 ]] || { usage; exit 2; }
case "${1:-}" in
  --upgrade) MODE=upgrade ;;
  --upgrade-custom|--upgrade-source) MODE=source ;;
  --backup) MODE=backup ;;
  --backup-db) MODE=database ;;
  --backup-images) MODE=backup; SAVE_IMAGES=1 ;;
  --check) MODE=check ;;
  --help|-h) usage; exit 0 ;;
  '')
    show_menu
    if [[ ! -t 0 ]]; then
      printf '\n未检测到交互终端，请在 SSH 终端运行，或指定操作参数。\n'
      usage
      exit 2
    fi
    while [[ -z "$MODE" ]]; do
      if ! read -r -p '请输入选项 [0-6] 并回车: ' choice; then
        printf '\n已取消。\n'; exit 0
      fi
      case "$choice" in
        1) MODE=upgrade ;; 2) MODE=backup ;; 3) MODE=database ;;
        4) MODE=backup; SAVE_IMAGES=1 ;; 5) MODE=check ;;
        6) MODE=source ;;
        0|q|Q) printf '已退出。\n'; exit 0 ;;
        *) printf '请输入 0-6；输入 0 退出。\n' ;;
      esac
    done ;;
  *) usage; exit 2 ;;
esac
log() { printf '[%s] %s\n' "$(date '+%F %T')" "$*"; }
die() { log "ERROR: $*" >&2; exit 1; }

finish() {
  local rc=$?
  trap - EXIT INT TERM
  if (( rc != 0 )); then
    log "FAILED (exit $rc). Backup: ${BACKUP:-not created}"
    if (( IMAGE_OVERRIDE_CHANGED == 1 && MIGRATION_POSSIBLE == 0 )); then
      if (( IMAGE_OVERRIDE_PRESENT == 1 )); then
        cp -a "$BACKUP/docker-compose.image.yml" "$IMAGE_OVERRIDE" || log 'Image override restore FAILED.'
      else
        rm -f -- "$IMAGE_OVERRIDE" || log 'Temporary image override removal FAILED.'
      fi
    fi
    if (( STOPPED == 1 && MIGRATION_POSSIBLE == 0 )); then
      log 'Upgrade has not started. Restarting the original container.'
      docker start newapi >/dev/null || log 'Original container restart FAILED.'
    elif (( MIGRATION_POSSIBLE == 1 )); then
      log 'New code may have migrated the database. No automatic downgrade or SQL restore.'
      log 'Inspect upgrade.log and newapi-after.log before choosing recovery.'
    fi
    if [[ -n "$BACKUP" ]]; then
      docker logs --tail 200 newapi > "$BACKUP/newapi-after.log" 2>&1 || true
    fi
  fi
  exit "$rc"
}
trap finish EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

[[ $EUID -eq 0 ]] || die 'Run as root on the US server.'
for cmd in docker python3 curl flock tar gzip df du mktemp awk grep tail sha256sum tee find xargs sort git; do
  command -v "$cmd" >/dev/null || die "Missing command: $cmd"
done
docker compose version >/dev/null
exec 9>/run/lock/rayapi-newapi-upgrade.lock
flock -n 9 || die 'Another upgrade/check is running.'
[[ -d "$DEPLOY" && ! -L "$DEPLOY" ]] || die 'Unexpected deployment directory.'
cd "$DEPLOY"
[[ -f docker-compose.yml && -f .env ]] || die 'docker-compose.yml or .env is missing.'
[[ ! -L "$CUSTOM_OVERRIDE" ]] || die 'Custom Compose override must not be a symlink.'
[[ ! -L "$IMAGE_OVERRIDE" ]] || die 'Image Compose override must not be a symlink.'

# Use the same project identity, and refuse unknown override-file deployments.
PROJECT=$(docker inspect newapi --format '{{ index .Config.Labels "com.docker.compose.project" }}')
FILES=$(docker inspect newapi --format '{{ index .Config.Labels "com.docker.compose.project.config_files" }}')
[[ -n "$PROJECT" && "$PROJECT" != '<no value>' ]] || die 'Container is not managed by Compose.'
BASE_FILES="$DEPLOY/docker-compose.yml"
CUSTOM_FILES="$BASE_FILES,$CUSTOM_OVERRIDE"
IMAGE_FILES="$BASE_FILES,$IMAGE_OVERRIDE"
ALL_FILES="$CUSTOM_FILES,$IMAGE_OVERRIDE"
case "$FILES" in
  "$BASE_FILES") ;;
  "$CUSTOM_FILES") [[ -f "$CUSTOM_OVERRIDE" ]] || die 'Running custom deployment has no override file.' ;;
  "$IMAGE_FILES") [[ -f "$IMAGE_OVERRIDE" ]] || die 'Running registry deployment has no image override.' ;;
  "$ALL_FILES") [[ -f "$CUSTOM_OVERRIDE" && -f "$IMAGE_OVERRIDE" ]] || die 'Running registry deployment has missing overrides.' ;;
  *) die "Unexpected Compose files: $FILES" ;;
esac
COMPOSE=(docker compose --project-directory "$DEPLOY" --env-file "$DEPLOY/.env" -p "$PROJECT" -f "$DEPLOY/docker-compose.yml")
if [[ -f "$CUSTOM_OVERRIDE" ]]; then
  COMPOSE+=(-f "$CUSTOM_OVERRIDE")
fi
if [[ -f "$IMAGE_OVERRIDE" ]]; then
  IMAGE_OVERRIDE_PRESENT=1
  COMPOSE+=(-f "$IMAGE_OVERRIDE")
  # This file is managed by this script and must not contain other overrides.
  python3 - "$IMAGE_OVERRIDE" <<'PY'
import pathlib, re, sys
content = pathlib.Path(sys.argv[1]).read_text()
if not re.fullmatch(r"services:\n  newapi:\n    image: ghcr\.io/luckkai1989/new-api@sha256:[0-9a-f]{64}\n", content):
    raise SystemExit("Image override contains unmanaged settings; keep custom settings in docker-compose.custom.yml")
PY
fi

# Parse configuration in memory; never print credentials from resolved Compose.
CONFIG_PREFLIGHT=$("${COMPOSE[@]}" config --format json | python3 -c '
import json, re, subprocess, sys
c = json.load(sys.stdin)
s = c["services"]
a = s["newapi"]
if sys.argv[1] == "upgrade":
    assert a.get("image") == "calciumion/new-api:latest", "Official upgrade requires no custom image override"
elif sys.argv[1] in ("source", "check"):
    image = a.get("image", "")
    assert image == "calciumion/new-api:latest" or image.startswith("rayapi/new-api:source-") or re.fullmatch(r"ghcr\.io/luckkai1989/new-api@sha256:[0-9a-f]{64}", image), "Unexpected NewAPI image"
if sys.argv[1] in ("upgrade", "source", "check"):
    assert not a.get("build"), "Source-built deployment requires a separate upgrade workflow"
assert a.get("container_name") == "newapi", "Unexpected application container"
assert s["mysql"].get("container_name") == "newapi-mysql", "Unexpected MySQL container"
assert s["redis"].get("container_name") == "newapi-redis", "Unexpected Redis container"
assert s["mysql"].get("environment", {}).get("MYSQL_DATABASE") == "newapi", "Unexpected database name"
dsn = a.get("environment", {}).get("SQL_DSN", "")
assert "@tcp(mysql:3306)/newapi?" in dsn or dsn.endswith("@tcp(mysql:3306)/newapi"), "Unexpected SQL_DSN target"
p = a.get("ports", [])
assert len(p) == 1 and str(p[0].get("published")) == "19733" and p[0].get("target") == 3000 and p[0].get("host_ip") == "127.0.0.1", "Expected ONLY 127.0.0.1:19733:3000; do not widen public exposure"
assert a.get("healthcheck"), "NewAPI healthcheck is required"
for v in a.get("volumes", []):
    assert v.get("type") == "bind" and v.get("source", "").startswith("/opt/newapi/"), "Application mount outside backup scope"
monitor_recreate = 0
if sys.argv[1] in ("source", "check"):
    expected = a.get("environment", {}).get("UPSTREAM_MONITOR_ENCRYPTION_KEY", "") or ""
    if len(expected.encode("utf-8")) < 32:
        raise SystemExit("Configure a persistent UPSTREAM_MONITOR_ENCRYPTION_KEY of at least 32 bytes in /opt/newapi/.env and pass it through Compose; no credentials will be printed")
    inspected = subprocess.run(["docker", "inspect", "newapi"], capture_output=True, text=True, check=True)
    entries = json.loads(inspected.stdout)[0]["Config"].get("Env", [])
    running = dict(item.split("=", 1) for item in entries if "=" in item).get("UPSTREAM_MONITOR_ENCRYPTION_KEY", "")
    if running and running != expected:
        raise SystemExit("Running monitor key differs from Compose; restore the existing key before upgrading, do not rotate it implicitly")
    monitor_recreate = int(running != expected)
print(a["image"] + "\t" + str(monitor_recreate))
' "$MODE")
IFS=$'\t' read -r CONFIG_IMAGE MONITOR_RECREATE <<< "$CONFIG_PREFLIGHT"
[[ "$MONITOR_RECREATE" == 0 || "$MONITOR_RECREATE" == 1 ]] || die 'Invalid monitor-key preflight result.'
log 'Compose preflight passed.'
if (( MONITOR_RECREATE )); then
  log 'Monitor encryption key is configured but not applied; this upgrade will recreate only NewAPI to inject it.'
fi
for name in newapi newapi-mysql newapi-redis; do
  [[ $(docker inspect "$name" --format '{{.State.Running}}') == true ]] || die "$name is not running."
done
OLD_IMAGE=$(docker inspect newapi --format '{{.Image}}')
RUNNING_REF=$(docker inspect newapi --format '{{.Config.Image}}')
[[ "$RUNNING_REF" == "$CONFIG_IMAGE" ]] || die 'Running image reference differs from current Compose configuration.'
if [[ "$MODE" == upgrade ]]; then
  [[ ! -f "$CUSTOM_OVERRIDE" && ! -f "$IMAGE_OVERRIDE" && "$RUNNING_REF" == calciumion/new-api:latest ]] || die 'Official upgrade is unavailable while a custom image override is active.'
fi
if [[ "$MODE" == source ]]; then
  [[ -d "$SOURCE" && ! -L "$SOURCE" && -f "$SOURCE/Dockerfile" && -f "$SOURCE/service/upstream_monitor.go" ]] || die "Expected checked-out custom source at $SOURCE"
  [[ $(git -C "$SOURCE" remote get-url origin) == https://github.com/luckkai1989/new-api.git ]] || die 'Unexpected custom source origin.'
  [[ $(git -C "$SOURCE" branch --show-current) == main ]] || die 'Custom source must be on the main branch.'
  git -C "$SOURCE" diff --quiet && git -C "$SOURCE" diff --cached --quiet || die 'Custom source has uncommitted tracked changes.'
  [[ -z $(git -C "$SOURCE" status --porcelain --untracked-files=normal) ]] || die 'Custom source has untracked or modified files; use a clean checkout.'
  PLATFORM=$(docker info --format '{{.OSType}}/{{.Architecture}}')
  [[ "$PLATFORM" == linux/x86_64 || "$PLATFORM" == linux/amd64 ]] || die 'The GitHub image currently supports only Linux amd64 servers.'
fi
OLD_VERSION=$(docker exec newapi /new-api --version)
OLD_VERSION=${OLD_VERSION:-unknown}
log "Current version: $OLD_VERSION"

docker exec newapi-mysql sh -c 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql -uroot -N -B newapi -e "SELECT 1"' >/dev/null
if [[ "$MODE" != database ]]; then
  REDIS_PING=$(docker exec newapi-redis sh -eu -c 'test -n "${REDISCLI_AUTH:-}"; command -v redis-check-rdb >/dev/null; redis-cli --no-auth-warning --raw PING')
  [[ "$REDIS_PING" == PONG ]] || die 'Redis credentials or snapshot tools are unavailable.'
fi
DB_BYTES=$(docker exec newapi-mysql sh -c 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql -uroot -N -B -e "SELECT COALESCE(SUM(data_length+index_length),0) FROM information_schema.tables WHERE table_schema = '\''newapi'\''"')
[[ "$DB_BYTES" =~ ^[0-9]+$ ]] || die 'Unable to estimate database size.'
APP_BYTES=$(du -sb "$DEPLOY" | awk '{print $1}')
FREE_KB=$(df -Pk /opt | awk 'END {print $4}')
NEEDED_KB=$(( (DB_BYTES * 3 + APP_BYTES * 2) / 1024 + 3 * 1024 * 1024 ))
if (( SAVE_IMAGES )); then
  for name in newapi newapi-mysql newapi-redis; do
    IMAGE_ID=$(docker inspect "$name" --format '{{.Image}}')
    IMAGE_BYTES=$(docker image inspect "$IMAGE_ID" --format '{{.Size}}')
    [[ "$IMAGE_BYTES" =~ ^[0-9]+$ ]] || die 'Unable to estimate image size.'
    NEEDED_KB=$(( NEEDED_KB + IMAGE_BYTES * 2 / 1024 ))
  done
fi
log "Disk preflight: /opt available $(awk -v n="$FREE_KB" 'BEGIN {printf "%.2f", n/1048576}') GiB; required > $(awk -v n="$NEEDED_KB" 'BEGIN {printf "%.2f", n/1048576}') GiB."
(( FREE_KB > NEEDED_KB )) || die 'Insufficient /opt free space for backup plus upgrade reserve.'
log 'Preflight passed: containers, DB access, Compose and backup disk space.'
if [[ "$MODE" == check ]]; then
  log 'Check complete. No image pulled, service stopped, or deployment changed.'
  exit 0
fi
if [[ "$MODE" == source ]]; then
  log 'Fast-forwarding the clean custom checkout from origin/main while the service remains online.'
  GIT_TERMINAL_PROMPT=0 git -C "$SOURCE" pull --ff-only origin main || die 'Custom source pull failed; service remains online.'
  [[ -z $(git -C "$SOURCE" status --porcelain --untracked-files=normal) ]] || die 'Custom source is not clean after pull; service remains online.'
  SOURCE_COMMIT=$(git -C "$SOURCE" rev-parse --verify HEAD)
  [[ "$SOURCE_COMMIT" =~ ^[0-9a-f]{40}$ ]] || die 'Invalid custom source commit.'
  [[ "$SOURCE_COMMIT" == "$(git -C "$SOURCE" rev-parse --verify refs/remotes/origin/main)" ]] || die 'Local main differs from origin/main; do not deploy unpublished commits.'
  SOURCE_TAG="$CUSTOM_IMAGE:sha-$SOURCE_COMMIT"
  log "Required GitHub image: $SOURCE_TAG"
fi

mkdir -p "$BACKUP_ROOT"
[[ ! -L "$BACKUP_ROOT" ]] || die 'Backup root must not be a symlink.'
chmod 700 "$BACKUP_ROOT"
BACKUP=$(mktemp -d "$BACKUP_ROOT/$(date +%Y%m%d-%H%M%S)-XXXXXX")
exec > >(tee -a "$BACKUP/upgrade.log") 2>&1
log "Backup directory: $BACKUP"
cp -a docker-compose.yml .env "$BACKUP/"
if [[ -f "$CUSTOM_OVERRIDE" ]]; then
  cp -a "$CUSTOM_OVERRIDE" "$BACKUP/"
fi
if (( IMAGE_OVERRIDE_PRESENT )); then
  cp -a "$IMAGE_OVERRIDE" "$BACKUP/"
fi
chmod 600 "$BACKUP/docker-compose.yml" "$BACKUP/.env"
cp -a /etc/nginx "$BACKUP/nginx"
printf '%s\n' "$MODE" > "$BACKUP/operation.txt"
printf '%s\n' "$OLD_VERSION" > "$BACKUP/version-before.txt"
printf '%s\n' "$OLD_IMAGE" > "$BACKUP/image-before.txt"
if [[ "$MODE" == source ]]; then
  printf '%s\n' "$SOURCE_COMMIT" > "$BACKUP/source-commit.txt"
  printf '%s\n' "$SOURCE_TAG" > "$BACKUP/source-image-tag.txt"
fi
OLD_TAG="newapi-local:before-$(basename "$BACKUP" | tr '[:upper:]' '[:lower:]')"
docker image tag "$OLD_IMAGE" "$OLD_TAG"
printf '%s\n' "$OLD_TAG" > "$BACKUP/old-image-tag.txt"
docker logs --tail 200 newapi > "$BACKUP/newapi-before.log" 2>&1
docker inspect newapi newapi-mysql newapi-redis > "$BACKUP/containers.inspect.json"
"${COMPOSE[@]}" config --format json > "$BACKUP/compose.resolved.json"

dump_mysql() {
  log 'Exporting MySQL database.'
  docker exec newapi-mysql sh -c 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysqldump -uroot --single-transaction --quick --routines --triggers --events --no-tablespaces --databases newapi' > "$BACKUP/newapi.sql.part"
  [[ -s "$BACKUP/newapi.sql.part" ]] || die 'Database dump is empty.'
  tail -n 5 "$BACKUP/newapi.sql.part" | grep -q 'Dump completed' || die 'Dump completion marker missing.'
  mv "$BACKUP/newapi.sql.part" "$BACKUP/newapi.sql"
}

seal_backup() {
  # Include hidden configuration files, but not the still-growing log or markers.
  (cd "$BACKUP" && find . -type f ! -name upgrade.log ! -name SHA256SUMS ! -name '*_COMPLETE' ! -name '*.part' -print0 | sort -z | xargs -0 sha256sum > SHA256SUMS)
  (cd "$BACKUP" && sha256sum -c SHA256SUMS >/dev/null)
  touch "$BACKUP/BACKUP_COMPLETE"
}

prune_completed_backups() {
  local name dir resolved kept=0 operation
  while IFS= read -r name; do
    [[ "$name" =~ ^[0-9]{8}-[0-9]{6}-[[:alnum:]]{6}$ ]] || continue
    dir="$BACKUP_ROOT/$name"
    [[ -d "$dir" && ! -L "$dir" && -f "$dir/BACKUP_COMPLETE" && -f "$dir/SHA256SUMS" && -f "$dir/operation.txt" ]] || continue
    operation=$(<"$dir/operation.txt")
    case "$operation" in
      database) [[ -f "$dir/newapi.sql" ]] || continue ;;
      upgrade|source|backup) [[ -f "$dir/UPGRADE_COMPLETE" || -f "$dir/SERVICE_RESUMED" ]] || continue ;;
      *) continue ;;
    esac
    kept=$((kept + 1))
    if (( kept <= KEEP_BACKUPS )); then
      continue
    fi
    if ! resolved=$(cd -P -- "$dir" && pwd -P); then
      log "WARNING: Cannot resolve old backup path; keeping $dir"
      continue
    fi
    if [[ "$resolved" != "$BACKUP_ROOT/$name" ]]; then
      log "WARNING: Backup path changed; keeping $dir"
      continue
    fi
    if rm -rf --one-file-system -- "$dir"; then
      log "Removed old completed backup: $dir"
    else
      log "WARNING: Could not remove old backup: $dir"
    fi
  done < <(find "$BACKUP_ROOT" -mindepth 1 -maxdepth 1 -type d -printf '%f\n' | LC_ALL=C sort -r)
}

if [[ "$MODE" == database ]]; then
  # A consistent online snapshot requires all application tables to use InnoDB.
  NON_INNODB=$(docker exec newapi-mysql sh -c 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql -uroot -N -B -e "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = '\''newapi'\'' AND table_type = '\''BASE TABLE'\'' AND engine <> '\''InnoDB'\''"')
  [[ "$NON_INNODB" == 0 ]] || die 'Online snapshot requires InnoDB tables. Use --backup during maintenance.'
  dump_mysql
  seal_backup
  log "DATABASE BACKUP COMPLETE: $BACKUP (service remained online; no Redis or image archive)"
  prune_completed_backups
  exit 0
fi

if [[ "$MODE" == upgrade || "$MODE" == source ]]; then
  if [[ "$MODE" == upgrade ]]; then
    log 'Pulling official latest image while the current service remains online.'
    "${COMPOSE[@]}" pull newapi
    TARGET_REF=calciumion/new-api:latest
  else
    log "Pulling GitHub-built image $SOURCE_TAG while the current service remains online."
    if ! docker pull --platform linux/amd64 "$SOURCE_TAG"; then
      die 'Image pull failed. Wait for the main commit Actions build to succeed; for private GHCR packages run docker login ghcr.io with read:packages access. No local build or older-image fallback is allowed.'
    fi
    IMAGE_METADATA=$(docker image inspect "$SOURCE_TAG")
    TARGET_REF=$(printf '%s' "$IMAGE_METADATA" | python3 -c '
import json, re, sys
image = json.load(sys.stdin)[0]
labels = image.get("Config", {}).get("Labels", {}) or {}
if labels.get("org.opencontainers.image.revision") != sys.argv[1]:
    raise SystemExit("Image revision differs from origin/main; refusing deployment")
if labels.get("org.opencontainers.image.source") != sys.argv[3]:
    raise SystemExit("Image is not attributed to the configured fork; refusing deployment")
if image.get("Os") != "linux" or image.get("Architecture") != "amd64":
    raise SystemExit("Unexpected image platform; refusing deployment")
digests = [d for d in image.get("RepoDigests", []) if re.fullmatch(re.escape(sys.argv[2]) + r"@sha256:[0-9a-f]{64}", d)]
if len(set(digests)) != 1:
    raise SystemExit("Missing or ambiguous GHCR image digest; refusing deployment")
print(digests[0])
' "$SOURCE_COMMIT" "$CUSTOM_IMAGE" "$CUSTOM_REPOSITORY")
    TAG_IMAGE=$(docker image inspect "$SOURCE_TAG" --format '{{.Id}}')
    [[ $(docker image inspect "$TARGET_REF" --format '{{.Id}}') == "$TAG_IMAGE" ]] || die 'Digest does not resolve to the verified image.'
    printf '%s\n' "$TARGET_REF" > "$BACKUP/source-image-digest.txt"
    printf '%s\n' "$IMAGE_METADATA" > "$BACKUP/source-image.inspect.json"
    log "Verified source commit: $SOURCE_COMMIT; pinned image: $TARGET_REF"
  fi
  NEW_IMAGE=$(docker image inspect "$TARGET_REF" --format '{{.Id}}')
  printf '%s\n' "$NEW_IMAGE" > "$BACKUP/image-target.txt"
  if [[ "$OLD_IMAGE" == "$NEW_IMAGE" && "$MONITOR_RECREATE" == 0 ]]; then
    log 'Already on this image. No restart or database backup required.'
    exit 0
  fi
  # Pulling an image must not consume the space reserved for consistent backups.
  FREE_KB=$(df -Pk /opt | awk 'END {print $4}')
  (( FREE_KB > NEEDED_KB )) || die 'Insufficient backup space after image pull; the current service remains online.'
fi

# Save images before the outage; these are the running versions, not mutable tags.
if (( SAVE_IMAGES )); then
  IMAGE_IDS=()
  for name in newapi newapi-mysql newapi-redis; do
    IMAGE_IDS+=("$(docker inspect "$name" --format '{{.Image}}')")
  done
  log 'Exporting running NewAPI/MySQL/Redis images. This may take several minutes.'
  docker image save "${IMAGE_IDS[@]}" | gzip > "$BACKUP/container-images.tar.gz.part"
  gzip -t "$BACKUP/container-images.tar.gz.part"
  mv "$BACKUP/container-images.tar.gz.part" "$BACKUP/container-images.tar.gz"
fi

log 'Maintenance starts. Active requests may be interrupted; stop timeout is 60 seconds.'
STOPPED=1
"${COMPOSE[@]}" stop --timeout 60 newapi
[[ $(docker inspect newapi --format '{{.State.Running}}') == false ]] || die 'Application did not stop.'

# Quiesce the application before the snapshot so new billing writes cannot race it.
dump_mysql
log 'Exporting a Redis RDB snapshot using the container REDISCLI_AUTH.'
# redis-cli --rdb creates an independent snapshot; no SAVE, FLUSH or AOF rewrite.
docker exec newapi-redis sh -eu -c '
  test -n "${REDISCLI_AUTH:-}" || { echo "REDISCLI_AUTH is missing" >&2; exit 1; }
  # BusyBox mktemp requires the template to end with XXXXXX (no .rdb suffix).
  snapshot=$(mktemp /tmp/rayapi-backup-XXXXXX)
  trap '\''rm -f "$snapshot"'\'' EXIT
  redis-cli --no-auth-warning --rdb "$snapshot" >&2
  redis-check-rdb "$snapshot" >&2
  cat "$snapshot"
' > "$BACKUP/redis.rdb.part"
[[ -s "$BACKUP/redis.rdb.part" ]] || die 'Redis snapshot is empty.'
mv "$BACKUP/redis.rdb.part" "$BACKUP/redis.rdb"
log 'Archiving application data/configuration/logs.'
tar -czf "$BACKUP/newapi-files.tar.gz" -C /opt newapi
gzip -t "$BACKUP/newapi-files.tar.gz"
seal_backup

if [[ "$MODE" == backup ]]; then
  log 'Backup complete. Starting the original NewAPI container without upgrading.'
  docker start newapi >/dev/null
else
  if [[ "$MODE" == source ]]; then
    override_part=$(mktemp "$DEPLOY/.docker-compose.image-XXXXXX")
    printf 'services:\n  newapi:\n    image: %s\n' "$TARGET_REF" > "$override_part"
    mv -f "$override_part" "$IMAGE_OVERRIDE"
    IMAGE_OVERRIDE_CHANGED=1
    chmod 600 "$IMAGE_OVERRIDE"
    if (( IMAGE_OVERRIDE_PRESENT == 0 )); then
      COMPOSE+=(-f "$IMAGE_OVERRIDE")
    fi
    "${COMPOSE[@]}" config --quiet
  fi
  log 'Starting new image; MySQL and Redis will not be recreated.'
  MIGRATION_POSSIBLE=1
  "${COMPOSE[@]}" up -d --no-deps --pull never --force-recreate newapi
  [[ $(docker inspect newapi --format '{{.Image}}') == "$NEW_IMAGE" ]] || die 'Running image does not match pulled image.'
  [[ $(docker inspect newapi --format '{{.Config.Image}}') == "$TARGET_REF" ]] || die 'Running image reference does not match the selected image.'
fi
STOPPED=0
log 'Waiting approximately 180 seconds for container health and local API status.'
READY=0
DEADLINE=$((SECONDS + 180))
while (( SECONDS < DEADLINE )); do
  HEALTH=$(docker inspect newapi --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}missing{{end}}')
  if [[ "$HEALTH" == healthy ]] && curl -fsS --max-time 3 http://127.0.0.1:19733/api/status | python3 -c 'import json,sys; d=json.load(sys.stdin); sys.exit(0 if d.get("success") is True else 1)' 2>/dev/null; then
    READY=1
    break
  fi
  sleep 5
done
(( READY )) || die 'Health verification failed. New version may have changed the database.'
if [[ "$MODE" == source ]]; then
  docker exec newapi sh -eu -c 'test -n "${UPSTREAM_MONITOR_ENCRYPTION_KEY:-}" && test "${#UPSTREAM_MONITOR_ENCRYPTION_KEY}" -ge 32' || die 'Monitor encryption key was not injected into the new container.'
  log 'Upstream monitor encryption key validation passed; secret value is not printed.'
fi
NEW_VERSION=$(docker exec newapi /new-api --version)
NEW_VERSION=${NEW_VERSION:-unknown}
printf '%s\n' "$NEW_VERSION" > "$BACKUP/version-after.txt"
docker logs --tail 200 newapi > "$BACKUP/newapi-after.log" 2>&1
if [[ "$MODE" == upgrade || "$MODE" == source ]]; then
  touch "$BACKUP/UPGRADE_COMPLETE"
  log "UPGRADE SUCCESS: $OLD_VERSION -> $NEW_VERSION"
  if [[ "$MODE" == source ]]; then
    log "Deployed GitHub source commit: $SOURCE_COMMIT"
    log "Deployed immutable image: $TARGET_REF"
  fi
else
  touch "$BACKUP/SERVICE_RESUMED"
  log "BACKUP SUCCESS: original service resumed ($NEW_VERSION)"
fi
log "Backup: $BACKUP"
log "Old image retained as: $OLD_TAG"
log 'Health checks passed. Verify login, balances and one real model request separately.'
log 'Do not restore SQL or downgrade blindly after migrations. Keeping the latest three completed script backups; failed and manual backups are untouched.'
prune_completed_backups
if [[ "$MODE" == upgrade || "$MODE" == source ]]; then
  log 'Pruning dangling Docker images after the successful upgrade; tagged rollback images are retained.'
  if ! docker image prune -f; then
    log 'WARNING: Dangling image cleanup failed; the upgrade remains successful.'
  fi
fi
