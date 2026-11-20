// Package logmgr 负责运行日志的文件落盘、轮转与过期清理，
// 同时托管数据库里调用流水 / 签到历史的定期清理。
//
// 设计要点：
//   - 进程日志和库内流水是两类东西：前者是标准库 log 的输出，按天 + 大小轮转；
//     后者是 usage_logs / checkin_history 两张表，按时间戳批量删除。
//   - 清理频率和保留天数都能在面板里改，改完立即生效并持久化到 settings 表。
//   - 任何一步失败都不能影响主流程：日志写盘失败就静默丢弃，只保留标准错误输出。
package logmgr

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/3166790565/raccoon-relay/internal/config"
	"github.com/3166790565/raccoon-relay/internal/store"
)

const currentName = "relay.log"

// Config 是日志管理的运行参数。零值经 Normalize 后会变成一套安全的默认值。
type Config struct {
	Enable         bool          // 是否写文件日志
	Dir            string        // 日志目录
	MaxSizeMB      int           // 单个文件大小上限（MB），超过即轮转
	MaxFiles       int           // 最多保留的归档文件数
	FileRetainDays int           // 归档文件保留天数，0 表示只按 MaxFiles 清理
	DBRetainDays   int           // 调用流水保留天数，0 表示不清理数据库
	CleanEnable    bool          // 是否自动清理
	CleanInterval  time.Duration // 自动清理间隔
}

// Normalize 把越界或缺失的值收敛到可用范围。
func (c Config) Normalize() Config {
	if c.MaxSizeMB <= 0 {
		c.MaxSizeMB = 32
	}
	if c.MaxFiles <= 0 {
		c.MaxFiles = 10
	}
	if c.FileRetainDays < 0 {
		c.FileRetainDays = 0
	}
	if c.DBRetainDays < 0 {
		c.DBRetainDays = 0
	}
	if c.CleanInterval <= 0 {
		c.CleanInterval = time.Hour
	}
	return c
}

// FileInfo 描述日志目录里的一个文件。
type FileInfo struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"modTime"`
	Current bool   `json:"current"`
}

// CleanResult 是一条清理结果，面板和日志都用它。
type CleanResult struct {
	At             int64  `json:"at"`
	RemovedFiles   int    `json:"removedFiles"`
	FreedBytes     int64  `json:"freedBytes"`
	RemovedUsage   int64  `json:"removedUsage"`
	RemovedCheckin int64  `json:"removedCheckin"`
	Error          string `json:"error,omitempty"`
}

// Manager 同时是 io.Writer：标准库 log 的输出经它分流到标准错误和文件。
type Manager struct {
	mu   sync.Mutex
	cfg  Config
	st   *store.Store
	base io.Writer

	file     *os.File
	fileSize int64
	openedAt time.Time

	lastClean  time.Time
	lastResult CleanResult
}

// ConfigOf 从服务配置推导日志管理参数，启动路径和面板保存后共用这一份映射。
func ConfigOf(cfg *config.Config) Config {
	return Config{
		Enable:         cfg.LogFileEnable,
		Dir:            cfg.LogDir,
		MaxSizeMB:      cfg.LogMaxSizeMB,
		MaxFiles:       cfg.LogMaxFiles,
		FileRetainDays: cfg.LogFileRetainDays,
		DBRetainDays:   cfg.LogDBRetainDays,
		CleanEnable:    cfg.LogCleanEnable,
		CleanInterval:  time.Duration(cfg.LogCleanIntervalM) * time.Minute,
	}
}

// New 构造一个 Manager。base 通常是 os.Stderr，文件日志关闭时原样透传。
func New(cfg Config, st *store.Store) *Manager {
	return &Manager{cfg: cfg.Normalize(), st: st, base: os.Stderr}
}

// Start 把全局 logger 的输出接管到 Manager，并启动自动清理循环。
func (m *Manager) Start(ctx context.Context) {
	log.SetOutput(m)
	m.applyLocked(time.Now())
	m.mu.Lock()
	cfg := m.cfg
	m.mu.Unlock()
	if cfg.Enable {
		log.Printf("[日志] 文件日志已启用：%s（单文件 %dMB / 保留 %d 天 / 最多 %d 个归档）",
			filepath.Join(cfg.Dir, currentName), cfg.MaxSizeMB, cfg.FileRetainDays, cfg.MaxFiles)
	} else {
		log.Println("[日志] 文件日志已关闭，仅输出到标准错误")
	}
	go m.loop(ctx)
}

// Close 关闭文件句柄。调用后不再写盘。
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closeFileLocked()
}

// Config 返回当前配置的副本。
func (m *Manager) Config() Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg
}

// Settings 返回面板需要的扁平配置项。
func (m *Manager) Settings() map[string]any {
	c := m.Config()
	return map[string]any{
		"logFileEnable":       c.Enable,
		"logDir":              c.Dir,
		"logMaxSizeMB":        c.MaxSizeMB,
		"logMaxFiles":         c.MaxFiles,
		"logFileRetainDays":   c.FileRetainDays,
		"logDBRetainDays":     c.DBRetainDays,
		"logCleanEnable":      c.CleanEnable,
		"logCleanIntervalMin": int(c.CleanInterval / time.Minute),
	}
}

// Update 在运行期替换配置。目录或开关变化时关闭旧句柄，下一次写入按新配置重新打开。
func (m *Manager) Update(cfg Config) {
	cfg = cfg.Normalize()
	m.mu.Lock()
	old := m.cfg
	m.cfg = cfg
	if !cfg.Enable || old.Dir != cfg.Dir {
		m.closeFileLocked()
	}
	if cfg.Enable {
		_ = m.ensureLocked(time.Now())
	}
	m.mu.Unlock()
	if cfg.Enable {
		log.Printf("[日志] 配置已更新：单文件 %dMB / 保留 %d 天 / 最多 %d 个归档 / 流水保留 %d 天",
			cfg.MaxSizeMB, cfg.FileRetainDays, cfg.MaxFiles, cfg.DBRetainDays)
	}
}

// Write 实现 io.Writer。标准错误始终写；文件日志失败不向调用方报错，
// 因为标准库 log 本身会忽略写入错误，没必要让它多一条噪音。
func (m *Manager) Write(p []byte) (int, error) {
	m.mu.Lock()
	enable := m.cfg.Enable && strings.TrimSpace(m.cfg.Dir) != ""
	if enable {
		_ = m.writeFileLocked(p, time.Now())
	}
	m.mu.Unlock()
	return m.base.Write(p)
}

func (m *Manager) applyLocked(now time.Time) {
	if m.cfg.Enable && strings.TrimSpace(m.cfg.Dir) != "" {
		_ = m.ensureLocked(now)
	}
}

func (m *Manager) maxBytesLocked() int64 {
	return int64(m.cfg.MaxSizeMB) << 20
}

func (m *Manager) writeFileLocked(p []byte, now time.Time) error {
	if err := m.ensureLocked(now); err != nil {
		return err
	}
	if m.fileSize > 0 && m.fileSize+int64(len(p)) > m.maxBytesLocked() {
		m.rotateLocked(now)
		if err := m.ensureLocked(now); err != nil {
			return err
		}
	}
	n, err := m.file.Write(p)
	m.fileSize += int64(n)
	return err
}

// ensureLocked 保证当前文件处于打开状态，并在跨天时先把昨天的文件归档。
func (m *Manager) ensureLocked(now time.Time) error {
	if err := os.MkdirAll(m.cfg.Dir, 0o750); err != nil {
		return err
	}
	path := filepath.Join(m.cfg.Dir, currentName)
	if m.file != nil {
		if sameDay(m.openedAt, now) {
			return nil
		}
		m.closeFileLocked()
		m.archiveLocked(path, m.openedAt)
	}
	// 上次进程退出时留下的当前文件，如果日期不是今天，先归档再新建，
	// 避免新日志接在旧文件后面让「按天保留」失真。
	if fi, err := os.Stat(path); err == nil && !sameDay(fi.ModTime(), now) {
		m.archiveLocked(path, fi.ModTime())
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	m.file = f
	m.openedAt = now
	if fi, err := f.Stat(); err == nil {
		m.fileSize = fi.Size()
	} else {
		m.fileSize = 0
	}
	return nil
}

func (m *Manager) rotateLocked(now time.Time) {
	path := filepath.Join(m.cfg.Dir, currentName)
	openedAt := m.openedAt
	m.closeFileLocked()
	m.archiveLocked(path, openedAt)
}

func (m *Manager) closeFileLocked() {
	if m.file != nil {
		_ = m.file.Close()
		m.file = nil
	}
	m.fileSize = 0
}

// archiveLocked 把当前文件改名成带时间戳的归档，并顺带按策略修剪。
func (m *Manager) archiveLocked(path string, stamp time.Time) {
	if _, err := os.Stat(path); err != nil {
		return
	}
	if stamp.IsZero() {
		stamp = time.Now()
	}
	dst := filepath.Join(m.cfg.Dir, "relay-"+stamp.Format("20060102-150405")+".log")
	dst = uniqueName(dst)
	if err := os.Rename(path, dst); err != nil {
		return
	}
	m.pruneLocked(time.Now())
}

// pruneLocked 按保留天数和最大归档数删除旧文件。返回删除个数与释放字节数。
// 注意：调用方持有锁，这里绝对不能写日志（会重入 Write）。
func (m *Manager) pruneLocked(now time.Time) (int, int64) {
	entries, err := os.ReadDir(m.cfg.Dir)
	if err != nil {
		return 0, 0
	}
	type arch struct {
		name string
		size int64
		mod  time.Time
	}
	var archives []arch
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name == currentName || !strings.HasPrefix(name, "relay-") || !strings.HasSuffix(name, ".log") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		archives = append(archives, arch{name: name, size: info.Size(), mod: info.ModTime()})
	}
	sort.Slice(archives, func(i, j int) bool { return archives[i].mod.Before(archives[j].mod) })

	var removed int
	var freed int64
	remaining := archives[:0]
	var cutoff time.Time
	if m.cfg.FileRetainDays > 0 {
		cutoff = now.AddDate(0, 0, -m.cfg.FileRetainDays)
	}
	for _, a := range archives {
		expired := !cutoff.IsZero() && a.mod.Before(cutoff)
		if expired {
			if os.Remove(filepath.Join(m.cfg.Dir, a.name)) == nil {
				removed++
				freed += a.size
			}
			continue
		}
		remaining = append(remaining, a)
	}
	// 数量上限：归档已按时间升序，超出的从最旧的开始删
	for len(remaining) > m.cfg.MaxFiles {
		a := remaining[0]
		remaining = remaining[1:]
		if os.Remove(filepath.Join(m.cfg.Dir, a.name)) == nil {
			removed++
			freed += a.size
		}
	}
	return removed, freed
}

// Files 返回日志目录里当前文件与归档文件的清单，当前文件排在最前。
func (m *Manager) Files() []FileInfo {
	m.mu.Lock()
	dir := m.cfg.Dir
	m.mu.Unlock()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []FileInfo
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name != currentName && (!strings.HasPrefix(name, "relay-") || !strings.HasSuffix(name, ".log")) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, FileInfo{
			Name: name, Size: info.Size(), ModTime: info.ModTime().UnixMilli(), Current: name == currentName,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Current != out[j].Current {
			return out[i].Current
		}
		return out[i].ModTime > out[j].ModTime
	})
	return out
}

// ReadFile 读取日志目录里的一个文件。name 必须是 Files() 返回过的名字，
// 这里再做一次 basename 校验，双重防止路径穿越。
func (m *Manager) ReadFile(name string) ([]byte, error) {
	if filepath.Base(name) != name || name == "." || name == ".." {
		return nil, fmt.Errorf("非法文件名: %s", name)
	}
	m.mu.Lock()
	dir := m.cfg.Dir
	m.mu.Unlock()
	return os.ReadFile(filepath.Join(dir, name))
}

// Status 汇总面板要展示的日志状态（配置、文件、数据库规模、上次清理结果）。
func (m *Manager) Status(ctx context.Context) map[string]any {
	files := m.Files()
	var fileBytes int64
	for _, f := range files {
		fileBytes += f.Size
	}
	m.mu.Lock()
	lastClean := m.lastClean
	lastResult := m.lastResult
	m.mu.Unlock()

	out := map[string]any{
		"config":          m.Settings(),
		"files":           files,
		"fileCount":       len(files),
		"fileBytes":       fileBytes,
		"lastClean":       lastClean.UnixMilli(),
		"lastCleanResult": lastResult,
	}
	if m.st != nil {
		if n, earliest, err := m.st.UsageStats(ctx); err == nil {
			out["usageCount"] = n
			out["usageEarliest"] = earliest
		} else {
			out["usageCount"] = -1
		}
		if n, earliest, err := m.st.CheckinStats(ctx); err == nil {
			out["checkinCount"] = n
			out["checkinEarliest"] = earliest
		} else {
			out["checkinCount"] = -1
		}
	}
	return out
}

// CleanNow 立即执行一次清理：文件按保留天数 / 归档数删除，数据库按流水保留天数删除。
// 返回的结果里带各项计数，面板直接展示。
func (m *Manager) CleanNow(ctx context.Context) CleanResult {
	m.mu.Lock()
	cfg := m.cfg
	res := CleanResult{At: time.Now().UnixMilli()}
	if cfg.Enable && cfg.Dir != "" {
		res.RemovedFiles, res.FreedBytes = m.pruneLocked(time.Now())
	}
	m.mu.Unlock()
	return m.finishClean(ctx, cfg, res)
}

func (m *Manager) finishClean(ctx context.Context, cfg Config, res CleanResult) CleanResult {
	if cfg.DBRetainDays > 0 && m.st != nil {
		cutoff := time.Now().AddDate(0, 0, -cfg.DBRetainDays)
		// 分批删到删干净为止：每次只删 1000 行是为了不长时间占住 SQLite 写锁，
		// 但如果只删一批就收工，积压永远追不上新写入，保留天数形同虚设。
		// 加一个批次上限，避免首次清理几十万行时把这次请求拖太久。
		const batch = 1000
		const maxBatches = 200
		for i := 0; i < maxBatches; i++ {
			if ctx.Err() != nil {
				break
			}
			n, err := m.st.PurgeUsageBefore(ctx, cutoff.UnixMilli(), batch)
			if err != nil {
				res.Error = err.Error()
				break
			}
			res.RemovedUsage += n
			if n < batch {
				break
			}
		}
		for i := 0; i < maxBatches; i++ {
			if ctx.Err() != nil {
				break
			}
			n, err := m.st.PurgeCheckinBefore(ctx, cutoff.UnixMilli(), batch)
			if err != nil {
				if res.Error == "" {
					res.Error = err.Error()
				}
				break
			}
			res.RemovedCheckin += n
			if n < batch {
				break
			}
		}
	}
	m.mu.Lock()
	m.lastClean = time.Now()
	m.lastResult = res
	m.mu.Unlock()

	if res.RemovedFiles > 0 || res.RemovedUsage > 0 || res.RemovedCheckin > 0 {
		log.Printf("[日志] 清理完成：删除归档 %d 个（%.1f MB），调用流水 %d 条，签到记录 %d 条",
			res.RemovedFiles, float64(res.FreedBytes)/1024/1024, res.RemovedUsage, res.RemovedCheckin)
	}
	if res.Error != "" {
		log.Printf("[日志] 清理时出错：%s", res.Error)
	}
	return res
}

func (m *Manager) loop(ctx context.Context) {
	// 启动时补一次清理，但要尊重「自动清理」开关：关掉以后重启不该偷偷删东西。
	if m.Config().CleanEnable {
		m.CleanNow(ctx)
	}

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.mu.Lock()
			cfg := m.cfg
			due := cfg.CleanEnable && time.Since(m.lastClean) >= cfg.CleanInterval
			m.mu.Unlock()
			if due {
				m.CleanNow(ctx)
			}
		}
	}
}

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

// uniqueName 在重名时追加 -1、-2…，避免同一秒内多次轮转互相覆盖。
func uniqueName(path string) string {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return path
	}
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	for i := 1; i < 1000; i++ {
		candidate := fmt.Sprintf("%s-%d%s", base, i, ext)
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
	return path
}
