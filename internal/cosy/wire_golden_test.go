package cosy

import (
	"encoding/json"
	"os"
	"testing"
)

// 本文件的样本全部来自 Qoder 0.4.8 客户端的真实抓包（MITM），
// 用来把「实测协议」钉死在测试里，防止有人把版本号/编码改回猜测值。

// TestDecodeRealRegionEndpoints 解码真实端点表响应。
// 这族响应即使没标 Encode 也是自定义字母表编码，解错会直接读成乱码。
func TestDecodeRealRegionEndpoints(t *testing.T) {
	raw, err := os.ReadFile("testdata/region_endpoints.encoded")
	if err != nil {
		t.Fatalf("读样本失败: %v", err)
	}
	plain, err := DecodeBody(raw)
	if err != nil {
		t.Fatalf("DecodeBody: %v", err)
	}
	var table struct {
		InferNodes   []struct{ URL string } `json:"inferNodes"`
		CenterNodes  []struct{ URL string } `json:"centerNodes"`
		OpenAPINodes []struct{ URL string } `json:"openapiNodes"`
		Security     []struct{ URL string } `json:"security"`
		Codebase     []struct{ URL string } `json:"codebase"`
		RemoteAgent  []struct{ URL string } `json:"remoteAgent"`
	}
	if err := json.Unmarshal(plain, &table); err != nil {
		t.Fatalf("端点表不是合法 JSON: %v", err)
	}
	want := map[string]string{
		"推理网关":  "https://api3.qoder.sh",
		"中心":    "https://center.qoder.sh",
		"openapi": "https://openapi.qoder.sh",
		"安全/遥测": "https://api2.qoder.sh",
		"代码库":   "https://repo2.qoder.sh",
		"远程代理":  "https://qts2.qoder.sh",
	}
	got := map[string]string{}
	if len(table.InferNodes) > 0 {
		got["推理网关"] = table.InferNodes[0].URL
	}
	if len(table.CenterNodes) > 0 {
		got["中心"] = table.CenterNodes[0].URL
	}
	if len(table.OpenAPINodes) > 0 {
		got["openapi"] = table.OpenAPINodes[0].URL
	}
	if len(table.Security) > 0 {
		got["安全/遥测"] = table.Security[0].URL
	}
	if len(table.Codebase) > 0 {
		got["代码库"] = table.Codebase[0].URL
	}
	if len(table.RemoteAgent) > 0 {
		got["远程代理"] = table.RemoteAgent[0].URL
	}
	for name, url := range want {
		if got[name] != url {
			t.Errorf("%s = %q, want %q", name, got[name], url)
		}
	}
}

// TestEncodeDecodeRoundTrip 编码/解码必须互逆（解码是后加的方向）。
func TestEncodeDecodeRoundTrip(t *testing.T) {
	cases := [][]byte{
		[]byte(`{"a":1}`),
		[]byte(`{"request_id":"4c8e0618-bc87-46b8-be90-cd917a341f1e","stream":true}`),
		[]byte(`{}`),
		[]byte(`[]`),
	}
	for _, src := range cases {
		encoded := EncodeBody(src)
		back, err := DecodeBody(encoded)
		if err != nil {
			t.Fatalf("DecodeBody(%q): %v", src, err)
		}
		if string(back) != string(src) {
			t.Errorf("回转不一致: %q -> %q -> %q", src, encoded, back)
		}
	}
}

// TestDecodeBodyRejectsGarbage 长度不合法的输入必须报错而不是返回半截。
func TestDecodeBodyRejectsGarbage(t *testing.T) {
	if _, err := DecodeBody([]byte("abc")); err == nil {
		t.Errorf("长度不是 4 的倍数应该报错")
	}
	if _, err := DecodeBody(nil); err == nil {
		t.Errorf("空输入应该报错")
	}
}

// TestCapturedConstants 锁住抓包结论：版本号与客户端类型。
// 这两个值是「旧实现 vs 真实上游」分歧最久的两个字段。
func TestCapturedConstants(t *testing.T) {
	if GatewayVersion != "1.1.69" {
		t.Errorf("GatewayVersion = %q, 实测为 1.1.69", GatewayVersion)
	}
	if ClientType != "10" {
		t.Errorf("ClientType = %q, 实测为 10", ClientType)
	}
	if BusinessProduct != "app" || BusinessType != "agent" || Scene != "app" {
		t.Errorf("业务头 = %q/%q/%q, 实测为 app/agent/app",
			BusinessProduct, BusinessType, Scene)
	}
}
