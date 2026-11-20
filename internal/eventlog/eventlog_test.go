package eventlog

import (
	"context"
	"testing"

	"github.com/3166790565/raccoon-relay/internal/store"
)

// 同一句话在内存里先聚合，flush 时只写一行、次数累加。
func TestRecorderAggregates(t *testing.T) {
	st, err := store.Open(t.TempDir(), "", true)
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	defer st.Close()

	r := New(st)
	ev := store.RuntimeEvent{Level: store.LevelWarn, Scope: store.ScopeRelay,
		Component: "engine", AccountID: "acc1", Message: "上游失败：429"}
	for i := 0; i < 5; i++ {
		r.Emit(ev)
	}
	r.Flush(context.Background())

	rows, sum, err := st.QueryEvents(context.Background(), store.EventFilter{Page: 1, PageSize: 50})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Total != 1 || len(rows) != 1 {
		t.Fatalf("5 条相同事件应聚合为 1 行，实际 %d 行 / total=%d", len(rows), sum.Total)
	}
	if rows[0].Count != 5 {
		t.Fatalf("次数应为 5，实际 %d", rows[0].Count)
	}
}

// 级别为空时按 info 处理，来源为空时落到 system，不能写出一条「无从分类」的日志。
func TestRecorderNormalizes(t *testing.T) {
	st, err := store.Open(t.TempDir(), "", true)
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	defer st.Close()

	r := New(st)
	r.Emit(store.RuntimeEvent{Message: "服务启动"})
	r.Flush(context.Background())

	rows, _, err := st.QueryEvents(context.Background(), store.EventFilter{Page: 1, PageSize: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Level != store.LevelInfo || rows[0].Scope != store.ScopeSystem {
		t.Fatalf("应归一化为 info/system，实际 %+v", rows)
	}
}

// 空消息不写库；空 Recorder 上调用不应 panic（引擎可能在没有 recorder 的单测里跑）。
func TestRecorderEdgeCases(t *testing.T) {
	var nilRec *Recorder
	nilRec.Emit(store.RuntimeEvent{Message: "x"})
	nilRec.Flush(context.Background())

	st, err := store.Open(t.TempDir(), "", true)
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	defer st.Close()
	r := New(st)
	r.Emit(store.RuntimeEvent{Level: store.LevelInfo})
	r.Flush(context.Background())
	if count, _, _ := st.EventStats(context.Background()); count != 0 {
		t.Fatalf("空消息不应写库，实际 %d 条", count)
	}
}
