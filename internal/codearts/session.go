package codearts

import (
	"crypto/sha256"
	"encoding/hex"
)

// AccountSessionIDs 由账号凭据派生稳定的会话号与对话号。
//
// 码道网关每账号最多 3 个并发会话，并按 x-ot-session-id 区分「这是不是同一次
// 会话」。会话号必须稳定（同一账号的每个请求都带同一个），对话号同理由
// 首条消息派生；两者都由账号种子与「codearts-session:」前缀哈希出来。
func AccountSessionIDs(cred Credential) (sessionID, chatID string) {
	seed := cred.SessionSeed
	if seed == "" {
		seed = firstNonEmpty(cred.DomainID, cred.UserID, cred.AccessKeyID)
	}
	sum := sha256.Sum256([]byte("codearts-session:" + seed))
	h := hex.EncodeToString(sum[:])
	return h[0:32], h[32:64]
}
