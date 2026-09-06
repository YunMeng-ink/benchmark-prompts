#!/usr/bin/env bash
# 一次性一致性备份：用源站自带的 VACUUM INTO 路径，不依赖 sqlite3 CLI，
# 也不需要停服务（WAL/在线状态下都能拿到一致快照）。
#
# 手工执行：sudo /data/bench/bin/bench-backup.sh
# 定时执行：见 deploy/bench-backup.timer（安装说明在 deploy/README.md）
#
# 变量可被 EnvironmentFile 覆盖，默认值按 /data 数据盘约定。
set -euo pipefail

BIN="${BENCH_BIN:-/data/bench/bin/bench-server}"
CFG="${BENCH_CONFIG:-/data/bench/config.yaml}"
DEST_DIR="${BENCH_BACKUP_DIR:-/data/bench/backups}"
KEEP_DAYS="${BENCH_BACKUP_KEEP_DAYS:-14}"

cd /
mkdir -p "$DEST_DIR"

stamp=$(date -u +%Y%m%d-%H%M%SZ)
tmp="$DEST_DIR/.bench-$stamp.db.part"
final="$DEST_DIR/bench-$stamp.db"

# 先写同目录临时文件再改名：同盘 rename 是原子的，
# 目录里出现的文件一定是完整的，不会被还原时读到半截。
trap 'rm -f "$tmp"' EXIT
"$BIN" -config "$CFG" -backup "$tmp"
mv "$tmp" "$final"
trap - EXIT
# 备份文件含全部用户数据与 Key 哈希，落盘后立即收紧权限。
chmod 600 "$final"

# 只留最近 KEEP_DAYS 天（按 mtime 天数，+N 意为“超过 N*24h”）。
find "$DEST_DIR" -maxdepth 1 -name 'bench-*.db' -type f \
	-mtime "+$((KEEP_DAYS - 1))" -print -delete | sed 's/^/  已删除旧备份 /'

printf '备份完成 %s（%s 字节）\n' "$final" "$(stat -c %s "$final")"
