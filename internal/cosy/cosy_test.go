package cosy

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// EncodeBody 的已知向量（与上游客户端行为对齐，逐字节比对）。
func TestEncodeBodyKnownVectors(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"hello":"world"}`, "NO%zBEw$uYByBEJbmYKnDS%r"},
		{`{"a":1}`, "ep$$BMn%mYKi"},
		{`{"model":"Auto"}`, "JHq*ep$$uoByBzg.mYKtu(CL"},
		{``, ``},
		{`x`, "$_$m"},
	}
	for _, tc := range cases {
		if got := string(EncodeBody([]byte(tc.in))); got != tc.want {
			t.Errorf("EncodeBody(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSignaturePathStripsAlgoAndQuery(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://api3.qoder.sh/algo/api/v2/model/list?Encode=1", "/api/v2/model/list"},
		{"https://api3.qoder.sh/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=x", "/api/v2/service/pro/sse/agent_chat_generation"},
		{"https://example.com/foo/bar", "/foo/bar"},
		{"/algo/api/v2/model/list", "/api/v2/model/list"},
	}
	for _, tc := range cases {
		got, err := SignaturePath(tc.in)
		if err != nil {
			t.Fatalf("SignaturePath(%q) 出错: %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("SignaturePath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// 身份 JSON 键序必须逐字节固定。
func TestIdentityJSONKeyOrder(t *testing.T) {
	got := identityJSON(Identity{UserID: "u1", AuthToken: "tok", Name: "n", Email: "e"})
	want := `{"uid":"u1","security_oauth_token":"tok","name":"n","aid":"","email":"e"}`
	if got != want {
		t.Fatalf("identityJSON = %q, want %q", got, want)
	}
}

// 签名串拼接顺序：payload \n key \n timestamp \n body \n sigPath。
func TestSignatureKnownVector(t *testing.T) {
	got := signature("PAYLOAD", "KEY", "1700000000", []byte("BODY"), "/api/v2/x")
	want := "b8c79304f066c909aee64fcc43268cd5"
	if got != want {
		t.Fatalf("signature = %q, want %q", got, want)
	}
}

func TestBuildAuthHeadersShape(t *testing.T) {
	body := EncodeBody([]byte(`{"model":"Auto"}`))
	headers, err := BuildAuthHeaders(body, "https://api3.qoder.sh/algo/api/v2/service/pro/sse/agent_chat_generation?Encode=1", Identity{
		UserID: "u-123", AuthToken: "dt-token", Name: "张三", Email: "a@b.c", MachineID: "m-1",
	})
	if err != nil {
		t.Fatalf("BuildAuthHeaders 出错: %v", err)
	}
	keyRaw, err := base64.StdEncoding.DecodeString(headers["Cosy-Key"])
	if err != nil {
		t.Fatalf("Cosy-Key 不是合法 base64: %v", err)
	}
	if len(keyRaw) != 128 {
		t.Errorf("RSA 密文长度 = %d, want 128", len(keyRaw))
	}
	auth := headers["Authorization"]
	if !strings.HasPrefix(auth, "Bearer COSY.") {
		t.Fatalf("Authorization 形态错误: %q", auth)
	}
	parts := strings.Split(auth, ".")
	if len(parts) != 3 {
		t.Fatalf("Authorization 段数错误: %q", auth)
	}
	payloadRaw, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("payload 不是合法 base64: %v", err)
	}
	if !strings.HasPrefix(string(payloadRaw), `{"version":"v1","requestId":"`) {
		t.Fatalf("payload 键序错误: %s", payloadRaw)
	}
	checks := map[string]string{
		"Cosy-User":         "u-123",
		"Cosy-Version":      GatewayVersion,
		"Cosy-Machineid":    "m-1",
		"Cosy-Machinetoken": "m-1",
		"Cosy-Machinetype":  "5",
		"Cosy-Clienttype":   "10",
		"Cosy-Sigpath":      "/api/v2/service/pro/sse/agent_chat_generation",
		"Cosy-Bodylength":   strconv.Itoa(len(body)),
		"Login-Version":     "v2",
	}
	for name, want := range checks {
		if got := headers[name]; got != want {
			t.Errorf("头 %s = %q, want %q", name, got, want)
		}
	}
	if headers["Cosy-Bodyhash"] != md5Hex(body) {
		t.Errorf("Cosy-Bodyhash 与编码体不匹配")
	}
	if headers["X-Request-Id"] == "" {
		t.Errorf("缺少 X-Request-Id")
	}
}

func TestBuildAuthHeadersRequiresIdentity(t *testing.T) {
	if _, err := BuildAuthHeaders(nil, "https://x/algo/y", Identity{AuthToken: "t"}); err == nil {
		t.Errorf("缺少 userId 应报错")
	}
	if _, err := BuildAuthHeaders(nil, "https://x/algo/y", Identity{UserID: "u"}); err == nil {
		t.Errorf("缺少 accessToken 应报错")
	}
}

func TestRandomUUIDFormat(t *testing.T) {
	id, err := RandomUUID()
	if err != nil {
		t.Fatalf("RandomUUID 出错: %v", err)
	}
	if len(id) != 36 || strings.Count(id, "-") != 4 || id[14] != '4' {
		t.Fatalf("UUID 形态错误: %q", id)
	}
}

// 身份 JSON 对特殊字符必须仍转义成合法 JSON。
func TestIdentityJSONEscapesValues(t *testing.T) {
	got := identityJSON(Identity{UserID: `a"b`, AuthToken: "t\n", Name: "名", Email: "e"})
	var parsed map[string]string
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("身份 JSON 不是合法 JSON: %v (%s)", err, got)
	}
	if parsed["uid"] != `a"b` || parsed["security_oauth_token"] != "t\n" {
		t.Fatalf("转义后的值不正确: %+v", parsed)
	}
}
