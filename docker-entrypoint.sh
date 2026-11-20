#!/bin/sh
# 容器入口：修正数据目录可写后降权执行。
#
# 为什么需要这一步：镜像构建时 chown /data 是无意义的——bind mount 一挂上去，
# 宿主目录的属主和权限会覆盖镜像里的设置。默认 ./data 由 Docker 创建为 root:root 0755，
# 而进程以 relay(10001) 运行，写库就会得到 "unable to open database file (14)"。
set -e

DATA_DIR="${DATA_DIR:-/data}"

# 面板「一键更新」会把新二进制装到数据卷里（/data/bin/relay）。数据卷是持久化的，
# 所以每次启动优先跑它；想回到镜像自带的版本，删掉这个文件重启即可。
# 只在默认 CMD（/app/relay，无额外参数）时接管，不覆盖用户自定义的启动命令。
if [ -x "$DATA_DIR/bin/relay" ]; then
    if [ $# -eq 0 ]; then
        set -- "$DATA_DIR/bin/relay"
    elif [ $# -eq 1 ] && [ "$1" = "/app/relay" ]; then
        set -- "$DATA_DIR/bin/relay"
    fi
fi

if [ "$(id -u)" = "0" ]; then
    mkdir -p "$DATA_DIR"

    # 只给数据目录本身和 SQLite 文件改属主：
    #   · 不对整个目录 -R，避免把用户放在里面的其它文件一并改掉
    #   · 目录必须可写，SQLite 要在里面建 relay.db-wal / relay.db-shm
    chown relay:relay "$DATA_DIR" 2>/dev/null || true
    for f in "$DATA_DIR"/relay.db "$DATA_DIR"/relay.db-wal "$DATA_DIR"/relay.db-shm; do
        [ -e "$f" ] && chown relay:relay "$f" 2>/dev/null || true
    done

    # 如果宿主机文件系统不支持改属主（NFS / 某些 NAS 卷），退一步保证目录可写
    if [ ! -w "$DATA_DIR" ] || [ "$(stat -c %u "$DATA_DIR" 2>/dev/null)" != "10001" ]; then
        chmod 0775 "$DATA_DIR" 2>/dev/null || true
    fi

    if command -v su-exec >/dev/null 2>&1; then
        exec su-exec relay:relay "$@"
    elif command -v setpriv >/dev/null 2>&1; then
        exec setpriv --reuid=relay --regid=relay --clear-groups "$@"
    else
        exec su relay -c "$*"
    fi
fi

# 已经以非 root 启动（比如 compose 里指定了 user:）。
# 此时没有改属主的权限，只能先把问题说清楚，而不是让它变成一个看不懂的 SQLite 错误码。
if [ ! -w "$DATA_DIR" ]; then
    echo "[entrypoint] 错误：数据目录 $DATA_DIR 当前用户（uid $(id -u)）不可写。" >&2
    echo "[entrypoint] 容器以非 root 启动，无法自动修正。请在宿主机执行：" >&2
    echo "[entrypoint]   sudo chown -R $(id -u):$(id -g) <挂载到 $DATA_DIR 的宿主目录>" >&2
    exit 1
fi
exec "$@"
