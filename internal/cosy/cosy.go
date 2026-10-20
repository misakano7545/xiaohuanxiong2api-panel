// Package cosy 实现 Qoder 推理网关（/algo/... 一族）的 COSY 自签名鉴权与请求体编码。
//
// 推理网关不认普通 Bearer：它要一组 Cosy-* 头，其中 Authorization 是
// 「一次性 AES 密钥加密身份 JSON + 内置 RSA 公钥加密该密钥 + MD5 签名」。
// 签名覆盖**编码之后**的请求体，所以调用顺序必须是先 EncodeBody 再
// BuildAuthHeaders，颠倒会得到一个从错误里看不出原因的签名不匹配。
//
// 身份 JSON 与 payload 的键序逐字节参与签名，所以这里手写字符串拼接而不用
// map 序列化（Go 的 map 键序随机）。
package cosy

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"
)

const (
	// GatewayVersion 是推理网关使用的 COSY 协议版本。
	// 实测（Qoder 0.4.8 抓包）：1.1.69。
	GatewayVersion = "1.1.69"
	// ClientType 是 COSY 客户端类型；实测推理/queue/model list 全为 10。
	ClientType = "10"
	// DataPolicy 表示不同意把数据用于训练（chat 前客户端会读
	// config/getDataPolicy，用户同意后为 agree）。
	DataPolicy = "disagree"
	// LoginVersion 是登录版本标识。
	LoginVersion = "v2"

	// BusinessProduct / BusinessType / Scene 是推理网关校验的业务头，
	// 实测 chat 请求三者同时出现，缺任意一个都会被拒。
	BusinessProduct = "app"
	BusinessType    = "agent"
	Scene           = "app"

	machineType = "5"
)

// rsaModulusHex 是内置 RSA 公钥模数（1024 位，hex，无前导零）。它只用于加密
// 一次性 AES 密钥，不是机密材料；指数固定 65537。
const rsaModulusHex = "c0f22307e5cd362e296bb04470f6de8fbf935ce24e8fcf511a0e2701329769c4a76e499bb938036a52af1eaf818cf79a2600620e3ce87e371d2ca6d85803606a1b3fa5e874643c9ed2db7e85673ef7227fca56e2e7c08f0927609bb896a9f24be1782099a66016a5bfdc3f1ff756bfc9e88d7b5dc5be30bf45a0223a00ebcecf"

const rsaExponent = 65537

var (
	stdAlphabet    = []byte("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/")
	customAlphabet = []byte("_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!")
)

// Identity 是一次 COSY 签名所需的身份素材。
type Identity struct {
	UserID    string
	AuthToken string
	Name      string
	Email     string
	MachineID string
}

// EncodeBody 实现上游带 Encode=1 时的请求体编码：base64 → 尾/中/首重排 →
// 自定义字母表替换（含把 '=' 换成 '$'）。三步都是纯字节变换。
func EncodeBody(payload []byte) []byte {
	standard := base64.StdEncoding.EncodeToString(payload)
	length := len(standard)
	third := length / 3

	var table [256]byte
	for i := range table {
		table[i] = byte(i)
	}
	for i, ch := range stdAlphabet {
		table[ch] = customAlphabet[i]
	}
	table['='] = '$'

	out := make([]byte, 0, length)
	for i := length - third; i < length; i++ {
		out = append(out, table[standard[i]])
	}
	for i := third; i < length-third; i++ {
		out = append(out, table[standard[i]])
	}
	for i := 0; i < third; i++ {
		out = append(out, table[standard[i]])
	}
	return out
}

// DecodeBody 是 EncodeBody 的逆变换：端点表
// （/algo/api/v5/service/region/endpoints）即使在没标 Encode 的情况下
// 也返回编码后的正文，必须先解码再当 JSON 读。
func DecodeBody(encoded []byte) ([]byte, error) {
	length := len(encoded)
	if length == 0 || length%4 != 0 {
		return nil, fmt.Errorf("COSY 正文长度不是 4 的倍数")
	}
	third := length / 3

	// 编码时的顺序：尾段 (last third) + 中段 + 首段。
	std := make([]byte, length)
	copy(std[length-third:], encoded[:third])
	copy(std[third:length-third], encoded[third:length-third])
	copy(std[:third], encoded[length-third:])

	rev := make(map[byte]byte, 65)
	for i, ch := range stdAlphabet {
		rev[customAlphabet[i]] = ch
	}
	rev['$'] = '='
	for i, b := range std {
		if mapped, ok := rev[b]; ok {
			std[i] = mapped
		}
	}
	out, err := base64.StdEncoding.DecodeString(string(std))
	if err != nil {
		return nil, fmt.Errorf("COSY 正文 base64 解码失败")
	}
	return out, nil
}

// BuildAuthHeaders 构造一次请求所需的全部 COSY 头。body 必须是**已编码**的
// 请求体（GET 类请求传 nil），签名覆盖它。
func BuildAuthHeaders(body []byte, rawURL string, id Identity) (map[string]string, error) {
	if id.UserID == "" {
		return nil, fmt.Errorf("Qoder 账号缺少用户标识，无法签名")
	}
	if id.AuthToken == "" {
		return nil, fmt.Errorf("Qoder 账号缺少访问令牌，请重新登录")
	}
	secret, err := RandomSecret()
	if err != nil {
		return nil, err
	}
	aesKey := secret[:16]

	info, err := aes128CBCBase64(aesKey, []byte(identityJSON(id)))
	if err != nil {
		return nil, err
	}
	cosyKey, err := rsaEncryptBase64(aesKey)
	if err != nil {
		return nil, err
	}
	requestID, err := RandomUUID()
	if err != nil {
		return nil, err
	}
	timestamp := fmt.Sprintf("%d", time.Now().Unix())

	payloadSrc := `{"version":"v1","requestId":"` + requestID +
		`","info":"` + info + `","cosyVersion":"` + GatewayVersion + `","ideVersion":""}`
	payload := base64.StdEncoding.EncodeToString([]byte(payloadSrc))

	sigPath, err := SignaturePath(rawURL)
	if err != nil {
		return nil, err
	}
	if body == nil {
		body = []byte{}
	}
	signature := signature(payload, cosyKey, timestamp, body, sigPath)
	bodyHash := md5Hex(body)

	return map[string]string{
		"Authorization":          "Bearer COSY." + payload + "." + signature,
		"Cosy-Key":               cosyKey,
		"Cosy-User":              id.UserID,
		"Cosy-Date":              timestamp,
		"Cosy-Version":           GatewayVersion,
		"Cosy-Machineid":         id.MachineID,
		"Cosy-Machinetoken":      id.MachineID,
		"Cosy-Machinetype":       machineType,
		"Cosy-Machineos":         MachineOS(),
		"Cosy-Clienttype":        ClientType,
		// 实测 Cosy-ClientIp 装的是设备指纹 uuid，不是回环地址。
		"Cosy-Clientip":          id.MachineID,
		"Cosy-Business-Product":  BusinessProduct,
		"Cosy-Business-Type":     BusinessType,
		"Cosy-Scene":             Scene,
		"Cosy-Machinehostname":   machineHostname(),
		"Cosy-Bodyhash":          bodyHash,
		"Cosy-Bodylength":        fmt.Sprintf("%d", len(body)),
		"Cosy-Sigpath":           sigPath,
		"Cosy-Data-Policy":       DataPolicy,
		"Cosy-Organization-Id":   "",
		"Cosy-Organization-Tags": "",
		"Login-Version":          LoginVersion,
		"X-Request-Id":           requestID,
	}, nil
}

// identityJSON 按固定键序拼身份 JSON：uid / security_oauth_token / name /
// aid / email。各值单独做 JSON 转义，值里含引号或中文也不会拼坏。
func identityJSON(id Identity) string {
	return `{"uid":` + jsonText(id.UserID) +
		`,"security_oauth_token":` + jsonText(id.AuthToken) +
		`,"name":` + jsonText(id.Name) +
		`,"aid":""` +
		`,"email":` + jsonText(id.Email) + `}`
}

// signature 计算 MD5(payload \n key \n timestamp \n body \n sigPath) 的小写 hex。
func signature(payload, cosyKey, timestamp string, body []byte, sigPath string) string {
	h := md5.New()
	h.Write([]byte(payload))
	h.Write([]byte("\n"))
	h.Write([]byte(cosyKey))
	h.Write([]byte("\n"))
	h.Write([]byte(timestamp))
	h.Write([]byte("\n"))
	h.Write(body)
	h.Write([]byte("\n"))
	h.Write([]byte(sigPath))
	return hex.EncodeToString(h.Sum(nil))
}

func md5Hex(data []byte) string {
	sum := md5.Sum(data)
	return hex.EncodeToString(sum[:])
}

// SignaturePath 返回参与签名的路径：去掉 /algo 前缀、不含查询串。
func SignaturePath(rawURL string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("Qoder 请求地址无效")
	}
	path := parsed.Path
	if strings.HasPrefix(path, "/algo") {
		return strings.TrimPrefix(path, "/algo"), nil
	}
	return path, nil
}

// machineHostname 返回主机名（实测 chat 请求会带 Cosy-MachineHostname）。
func machineHostname() string {
	name, err := os.Hostname()
	if err != nil || name == "" {
		return "unknown"
	}
	return name
}

// MachineOS 返回 {arch}_{os} 形态的机器标识，arch 用 Qoder 的命名。
func MachineOS() string {
	arch := runtime.GOARCH
	switch arch {
	case "amd64":
		arch = "x86_64"
	case "arm64":
		arch = "aarch64"
	}
	return arch + "_" + runtime.GOOS
}

func jsonText(value string) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return `""`
	}
	return string(raw)
}

// RandomSecret 生成一段不可预测的随机串（base64url 无填充）。
func RandomSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("无法生成 Qoder 签名密钥")
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// RandomUUID 生成标准 uuid4（带连字符）。
func RandomUUID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("无法生成 Qoder 请求标识")
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16]), nil
}

// aes128CBCBase64 用 key 同时作为 IV，做 AES-128-CBC + PKCS#7 后 base64。
// 密钥与 IV 相同是上游既有做法，不是这里的设计选择。
func aes128CBCBase64(key16 string, plaintext []byte) (string, error) {
	if len(key16) != 16 {
		return "", fmt.Errorf("Qoder 签名密钥长度无效")
	}
	block, err := aes.NewCipher([]byte(key16))
	if err != nil {
		return "", fmt.Errorf("无法初始化 Qoder 签名算法")
	}
	padded := pkcs7Pad(plaintext, aes.BlockSize)
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, []byte(key16)).CryptBlocks(out, padded)
	return base64.StdEncoding.EncodeToString(out), nil
}

func pkcs7Pad(data []byte, blockSize int) []byte {
	pad := blockSize - len(data)%blockSize
	out := make([]byte, len(data)+pad)
	copy(out, data)
	for i := len(data); i < len(out); i++ {
		out[i] = byte(pad)
	}
	return out
}

// rsaEncryptBase64 按 PKCS#1 v1.5 打包后做一次模幂，输出定长 128 字节再
// base64。PS 必须是非零随机字节：固定 PS 会让 RSA 退化成确定性加密。
func rsaEncryptBase64(plaintext string) (string, error) {
	modulus, ok := new(big.Int).SetString(rsaModulusHex, 16)
	if !ok {
		return "", fmt.Errorf("Qoder 签名公钥无效")
	}
	keyLen := (modulus.BitLen() + 7) / 8
	message := []byte(plaintext)
	if len(message)+11 > keyLen {
		return "", fmt.Errorf("Qoder 签名内容过长")
	}
	psLen := keyLen - 3 - len(message)
	ps := make([]byte, psLen)
	if _, err := rand.Read(ps); err != nil {
		return "", fmt.Errorf("无法生成 Qoder 签名随机填充")
	}
	for i := range ps {
		if ps[i] == 0 {
			ps[i] = 1
		}
	}
	encoded := make([]byte, 0, keyLen)
	encoded = append(encoded, 0x00, 0x02)
	encoded = append(encoded, ps...)
	encoded = append(encoded, 0x00)
	encoded = append(encoded, message...)

	cipherInt := new(big.Int).SetBytes(encoded)
	cipherInt.Exp(cipherInt, big.NewInt(rsaExponent), modulus)
	raw := cipherInt.Bytes()
	if len(raw) > keyLen {
		return "", fmt.Errorf("Qoder 签名结果长度异常")
	}
	out := make([]byte, keyLen)
	copy(out[keyLen-len(raw):], raw)
	return base64.StdEncoding.EncodeToString(out), nil
}
