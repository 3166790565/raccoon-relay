//go:build !windows

package updater

import (
	"log"
	"os"
	"syscall"
	"time"
)

// Restart 用新二进制替换进程映像。
//
// 优先 exec 安装目标（容器里是数据卷中的 /data/bin/relay）：entrypoint 只在
// 容器启动时挑选二进制，execve 不会重新走它，所以这里必须自己指向新文件。
// 目标不可执行时退回 os.Executable()，保证裸机与自定义 UPDATE_TARGET 都能工作。
func Restart(delay time.Duration, target string) error {
	time.Sleep(delay)
	exe := target
	if exe == "" {
		if e, err := os.Executable(); err == nil {
			exe = e
		}
	}
	if info, err := os.Stat(exe); err != nil || info.IsDir() {
		// 没有安装目标（首次更新失败、或调用方没传）时按当前进程重启。
		if e, err := os.Executable(); err == nil {
			exe = e
		} else {
			return err
		}
	}
	argv := os.Args
	argv[0] = exe
	log.Printf("[update] 正在以新版本重启: %s", exe)
	if err := syscall.Exec(exe, argv, os.Environ()); err != nil {
		// 目标不可执行（权限/架构不符）：退回原路径，至少别让服务停在这里。
		if cur, cerr := os.Executable(); cerr == nil && cur != exe {
			log.Printf("[update] 启动安装目标失败（%v），退回当前程序 %s", err, cur)
			argv[0] = cur
			return syscall.Exec(cur, argv, os.Environ())
		}
		return err
	}
	return nil
}
