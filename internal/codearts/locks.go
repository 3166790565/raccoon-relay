package codearts

import "sync"

// refreshLocks 保证同一账号的续期串行。
//
// 多副本之间靠库里的 ext_rev CAS（store.ReplaceExtCredential）兜底；
// 进程内这一层只是减少无谓的上游往返与轮换冲突。
var refreshLocks sync.Map // accountID -> *sync.Mutex

func lockForAccount(id string) *sync.Mutex {
	v, _ := refreshLocks.LoadOrStore(id, &sync.Mutex{})
	return v.(*sync.Mutex)
}
