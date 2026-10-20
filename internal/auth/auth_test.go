package auth

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// makeJWT 造一个不验签的 JWT（payload 随便写）。
func makeJWT(payload map[string]any) string {
	raw, _ := json.Marshal(payload)
	body := base64.RawURLEncoding.EncodeToString(raw)
	return "h." + body + ".s"
}

func TestNewReadsJWTAndUid(t *testing.T) {
	access := makeJWT(map[string]any{"user_id": "u-123", "exp": 4102444800}) // 2100-01-01
	a := New("Bearer "+access, "refresh-1", "")
	if a.UID != "u-123" {
		t.Fatalf("uid = %q", a.UID)
	}
	if a.Name != "u-123" {
		t.Fatalf("空名字应回落到 uid: %q", a.Name)
	}
	if a.Expiry().Year() != 2100 {
		t.Fatalf("exp 解析失败: %v", a.Expiry())
	}
	if a.NeedsRefresh(0) {
		t.Fatalf("远期 token 不该需要续期")
	}
	// 没有 refresh_token 就无从续期：过期也不该报"需要刷新"，否则每个请求都白刷一次。
	expired := New(makeJWT(map[string]any{"exp": 1000}), "", "")
	if expired.NeedsRefresh(0) {
		t.Fatalf("无 refresh_token 时不该标记为需要续期")
	}
	if !New(makeJWT(map[string]any{"exp": 1000}), "refresh-1", "").NeedsRefresh(0) {
		t.Fatalf("过期且有 refresh_token 时应需要续期")
	}
}

func TestParseDefaultsEnabledTrue(t *testing.T) {
	a, err := Parse([]byte(`{"access_token":"x.y.z","name":"一号"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !a.Enabled {
		t.Fatalf("没写 enabled 时应视为启用")
	}
	b, err := Parse([]byte(`{"access_token":"x.y.z","enabled":false}`))
	if err != nil {
		t.Fatal(err)
	}
	if b.Enabled {
		t.Fatalf("显式 false 应保持停用")
	}
	if _, err := Parse([]byte(`{"name":"没有token"}`)); err == nil {
		t.Fatalf("缺 access_token 应报错")
	}
}

func TestSaveAtomicRoundTrip(t *testing.T) {
	dir := t.TempDir()
	a := New("access-1", "refresh-1", "一号")
	a.ApplyTokens("access-2", "refresh-2")
	a.SetCredit(42)
	if err := a.SaveAtomic(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, a.UID+".json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("账号文件权限 = %o，应为 600", perm)
	}
	list, err := LoadDir(dir)
	if err != nil || len(list) != 1 {
		t.Fatalf("回读失败: %v %d", err, len(list))
	}
	got := list[0]
	if got.Token() != "access-2" || got.RefreshValue() != "refresh-2" || got.Credit != 42 {
		t.Fatalf("回读内容不对: %+v", got.Snapshot())
	}
}
