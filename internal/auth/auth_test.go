package auth

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseDiskForm(t *testing.T) {
	raw := []byte(`{"mode":"cn","accessToken":"dt-1","refreshToken":"drt-1|u-9|m-9",
		"expiresAt":1792369369000,"userId":"u-9","email":"a@b.c","nickname":"阿九","machineId":"m-9"}`)
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse 出错: %v", err)
	}
	if a.UID != "u-9" || a.Realm() != RealmCN || a.MachineID != "m-9" {
		t.Fatalf("字段不对: %+v", a)
	}
	if a.ExpiresAt != 1792369369000 {
		t.Errorf("expiresAt = %d", a.ExpiresAt)
	}
	if got := a.OAuthRefresh(); got != "drt-1" {
		t.Errorf("OAuthRefresh = %q", got)
	}
}

func TestParsePAT(t *testing.T) {
	a, err := Parse([]byte(`{"pat":"pt-abc"}`))
	if err != nil {
		t.Fatalf("Parse 出错: %v", err)
	}
	if pat, ok := a.PAT(); !ok || pat != "pt-abc" {
		t.Fatalf("PAT = %q, %v", pat, ok)
	}
	if !a.CanRefresh() {
		t.Errorf("PAT 账号应可续期")
	}
}

func TestParseRejectsMismatchedIdentity(t *testing.T) {
	if _, err := Parse([]byte(`{"refreshToken":"drt|u-1|m-1","userId":"u-2"}`)); err == nil {
		t.Fatal("uid 与刷新串不一致应报错")
	}
}

func TestParseSecondsAndRFC3339(t *testing.T) {
	a, err := Parse([]byte(`{"accessToken":"x","userId":"u","machineId":"m","expiresAt":1792369369}`))
	if err != nil {
		t.Fatalf("Parse 出错: %v", err)
	}
	if a.ExpiresAt != 1792369369000 {
		t.Errorf("秒级时间戳应转成毫秒，得到 %d", a.ExpiresAt)
	}
	b, err := Parse([]byte(`{"accessToken":"x","userId":"u","machineId":"m","expires_at":"2026-10-19T08:00:00Z"}`))
	if err != nil {
		t.Fatalf("Parse 出错: %v", err)
	}
	want, _ := time.Parse(time.RFC3339, "2026-10-19T08:00:00Z")
	if b.ExpiresAt != want.UnixMilli() {
		t.Errorf("RFC3339 解析错误: %d", b.ExpiresAt)
	}
}

func TestCompleteIdentityRepacks(t *testing.T) {
	a := &Auth{RefreshToken: "drt|u-1|m-1", UID: "u-1", MachineID: "m-2"}
	if err := a.CompleteIdentity("fallback"); err != nil {
		t.Fatalf("CompleteIdentity 出错: %v", err)
	}
	if got := a.RefreshTokenValue(); got != "drt|u-1|m-2" {
		t.Errorf("刷新串后缀没改写: %q", got)
	}
	if got := a.OAuthRefresh(); got != "drt" {
		t.Errorf("OAuthRefresh = %q", got)
	}
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	a := &Auth{
		AccessToken: "dt", RefreshToken: "drt|u-7|m-7", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
		RealmID: RealmGlobal, MachineID: "m-7", UID: "u-7", Nickname: "七", Email: "q@w.e",
		FilePath: filepath.Join(dir, FileNameFor("u-7")),
	}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("SaveAtomic 出错: %v", err)
	}
	if info, err := os.Stat(a.FilePath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("落盘权限不是 0600: %v %v", info, err)
	}
	loaded, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir 出错: %v", err)
	}
	if len(loaded) != 1 || loaded[0].UID != "u-7" || loaded[0].MachineID != "m-7" {
		t.Fatalf("回读不一致: %+v", loaded)
	}
	if loaded[0].FilePath == "" {
		t.Errorf("回读后 FilePath 为空")
	}
}

func TestNeedsRefresh(t *testing.T) {
	a := &Auth{ExpiresAt: time.Now().Add(2 * time.Minute).UnixMilli()}
	if !a.NeedsRefresh(10 * time.Minute) {
		t.Errorf("2 分钟后过期、10 分钟窗口应判定需要刷新")
	}
	if a.NeedsRefresh(time.Minute) {
		t.Errorf("1 分钟窗口不该刷新")
	}
	if !(&Auth{}).NeedsRefresh(time.Minute) {
		t.Errorf("无 expiry 应判定需要刷新")
	}
}
