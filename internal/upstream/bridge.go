package upstream

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"

	"github.com/misakano7545/qoder2api-panel/internal/auth"
	"github.com/misakano7545/qoder2api-panel/internal/cosy"
)

// 本文件是 upstream 与 auth/cosy 之间的薄桥：协议细节各自留在自己的包里，
// 这里只做名字层面的转接，避免每个调用点都写两遍限定名。

func authParse(raw []byte) (*authState, error) { return auth.Parse(raw) }

func authPackRefresh(refresh, uid, machineID string) string {
	return auth.PackRefresh(refresh, uid, machineID)
}

func authPackPATRefresh(pat, jobRefresh, uid, machineID string) string {
	return auth.PackPATRefresh(pat, jobRefresh, uid, machineID)
}

// randomSecret / randomUUIDValue 复用 cosy 的随机源（同一强度）。
func randomSecret() (string, error) { return cosy.RandomSecret() }

func randomUUIDValue() (string, error) {
	uuid, err := cosy.RandomUUID()
	if err != nil {
		return "", err
	}
	return uuid, nil
}

// pkceChallenge 计算 S256 challenge（base64url 无填充）。
func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// mustJSON 只用于测试里的固定载荷。
func mustJSON(value any) []byte {
	raw, _ := json.Marshal(value)
	return raw
}
