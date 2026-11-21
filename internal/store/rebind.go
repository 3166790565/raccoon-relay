package store

import (
	"strconv"
	"strings"
)

// dialect 区分两种 SQL 方言。仓库里的 SQL 统一用 ? 占位符，
// Postgres 需要改写成 $1..$n，其余差异（DDL、ILIKE）由各自语句处理。
type dialect string

const (
	dialectSQLite   dialect = "sqlite"
	dialectPostgres dialect = "postgres"
)

// rebind 把 SQL 里的 ? 占位符改写为 Postgres 的 $n。
// SQLite 下原样返回：包装层不做任何字符串处理。
func (d dialect) rebind(q string) string {
	if d != dialectPostgres {
		return q
	}
	return rebindPostgres(q)
}

// rebindPostgres 单遍扫描，只改写字面量之外的 ?。
//
// 需要跳过的结构：
//   - '...' 字符串（含 ” 转义）
//   - "..." 标识符
//   - `...` 反引号标识符
//   - -- 行注释、/* */ 块注释
//   - $tag$ ... $tag$ / $$ ... $$ dollar-quoting
//
// 本仓库现有 SQL 已核对：没有 ? 字面量、没有 $ 占位符。
// 新写 SQL 时若混入 $n，Postgres 会直接报错，反而不会静默出错。
func rebindPostgres(q string) string {
	var b strings.Builder
	b.Grow(len(q) + 8)
	last, n := 0, 0
	for i := 0; i < len(q); i++ {
		switch q[i] {
		case '\'', '"', '`':
			i = skipQuoted(q, i, q[i])
		case '-':
			if i+1 < len(q) && q[i+1] == '-' {
				j := i + 2
				for j < len(q) && q[j] != '\n' {
					j++
				}
				i = j - 1
			}
		case '/':
			if i+1 < len(q) && q[i+1] == '*' {
				j := i + 2
				for j+1 < len(q) && !(q[j] == '*' && q[j+1] == '/') {
					j++
				}
				if j+1 < len(q) {
					i = j + 1
				} else {
					i = len(q) - 1
				}
			}
		case '$':
			if tag, ok := dollarTag(q, i); ok {
				if k := strings.Index(q[i+len(tag):], tag); k >= 0 {
					i = i + len(tag) + k + len(tag) - 1
				} else {
					i = len(q) - 1
				}
			}
		case '?':
			b.WriteString(q[last:i])
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			last = i + 1
		}
	}
	b.WriteString(q[last:])
	return b.String()
}

// skipQuoted 返回引号字面量结束位置的下标。未闭合时返回串尾。
func skipQuoted(q string, i int, quote byte) int {
	j := i + 1
	for j < len(q) {
		if q[j] == quote {
			if j+1 < len(q) && q[j+1] == quote {
				j += 2
				continue
			}
			return j
		}
		j++
	}
	return len(q) - 1
}

// dollarTag 识别 i 处的 dollar-quoting 起始标记（含两个 $）。
// 标签只允许字母、数字、下划线，且不得以数字开头；$$ 是空标签。
func dollarTag(q string, i int) (string, bool) {
	j := i + 1
	if j < len(q) && q[j] == '$' {
		return "$$", true
	}
	if j >= len(q) || !isTagStart(q[j]) {
		return "", false
	}
	for j < len(q) && isTagChar(q[j]) {
		j++
	}
	if j < len(q) && q[j] == '$' {
		return q[i : j+1], true
	}
	return "", false
}

func isTagStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isTagChar(c byte) bool {
	return isTagStart(c) || (c >= '0' && c <= '9')
}
