//go:build windows

package updater

import (
	"fmt"
	"time"
)

// Restart 在 Windows 上无法原地 exec：新文件已经替换到目标路径，
// 但当前进程还在运行旧映像。退出后由启动器（或手动）拉起即可。
func Restart(delay time.Duration, target string) error {
	return fmt.Errorf("Windows 下无法自动重启，请手动启动新版本（安装已完成）")
}
