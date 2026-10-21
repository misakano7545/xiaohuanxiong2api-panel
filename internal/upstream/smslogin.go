// smslogin.go 短信验证码登录：手机号加密 + 下发验证码 + 用验证码换凭证。
//
// 上游（抓包 + 同类实现实测）：
//
//	POST {auth_base}/send_sms            {"captcha_param":…, "nation_code":"86", "phone":"<密文>"}
//	POST {auth_base}/login_with_sms      {"nation_code":"86", "phone":"<密文>", "sms_code":"123456"}
//
// 两个硬约束（错误码可区分，见 TestSmsErrorSemantics 的注释）：
//
//  1. **手机号必须加密**，明文回 100003 params_encryted_error。算法照抄客户端渲染层：
//     key = UTF8("senseraccoon2023")（16 字节 ⇒ AES-128）、iv 随机 16 字节、CFB 模式、
//     NoPadding，输出 Base64(iv ‖ 密文)。
//  2. **captcha_param 必需**，缺了回 100006 captcha_verify_error。它是阿里云滑块的产物
//     （官方脚本 https://o.alicdn.com/captcha-frontend/aliyunCaptcha/AliyunCaptcha.js，
//     SceneId=1pkmy0x3、prefix=hk1r5l），由前端完成一次人机校验后拿到。
//
// 验证码发到用户自己手机上、由用户在面板里填回——本包只负责加密与转发，不碰验证码内容。
package upstream

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"

	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/auth"
)

const (
	SendSmsPath   = "/send_sms"
	LoginSmsPath  = "/login_with_sms"
	smsPhoneKey   = "senseraccoon2023" // 16 字节 → AES-128；照抄客户端常量
	smsNationCode = "86"               // 客户端下拉有 86/852/853/81，只做已验证的 86
)

// SmsPhoneCipherSecret 暴露密钥（仅供测试断言长度，业务代码别用）。
func SmsPhoneCipherSecret() string { return smsPhoneKey }

// EncryptPhone 加密手机号，产出 send_sms / login_with_sms 要的 Base64(iv‖密文)。
//
// 刻意用 crypto/rand：IV 重复会让相同号码产生相同密文，等于把用户手机号摊在日志里。
func EncryptPhone(phone string) (string, error) {
	return encryptPhoneWithIV(phone, nil)
}

// encryptPhoneWithIV iv 非空时使用它（仅供测试注入固定值）。
func encryptPhoneWithIV(phone string, iv []byte) (string, error) {
	phone = strings.TrimSpace(phone)
	if phone == "" {
		return "", errors.New("手机号不能为空")
	}
	key := []byte(smsPhoneKey)
	block, err := aes.NewCipher(key) // 16 字节密钥 ⇒ AES-128
	if err != nil {
		return "", err
	}
	if iv == nil {
		iv = make([]byte, aes.BlockSize)
		if _, err := rand.Read(iv); err != nil {
			return "", err
		}
	}
	if len(iv) != aes.BlockSize {
		return "", errors.New("IV 必须是 16 字节")
	}
	// CFB 是流密码：NoPadding 与默认输出一致（客户端用 CryptoJS NoPadding），
	// 这里不做 PKCS#7 填充——填充会让服务端解出一串尾部垃圾、号码被判非法。
	out := make([]byte, len(phone))
	cipher.NewCFBEncrypter(block, iv).XORKeyStream(out, []byte(phone))
	return base64.StdEncoding.EncodeToString(append(append([]byte{}, iv...), out...)), nil
}

// SendSmsCode 下发短信验证码。captchaParam 是阿里云滑块的产物（前端提供，必需）。
func (c *Client) SendSmsCode(phone, captchaParam string) error {
	enc, err := EncryptPhone(phone)
	if err != nil {
		return &Error{Kind: ErrClient, Status: 400, Msg: err.Error()}
	}
	resp, err := c.doJSON(http.MethodPost, c.authBase()+SendSmsPath, map[string]string{
		"captcha_param": strings.TrimSpace(captchaParam),
		"nation_code":   smsNationCode,
		"phone":         enc,
	}, c.clientIDHeaders())
	if err != nil {
		return err
	}
	if code, ok := businessCodeOf(resp.Body); ok && code != 0 {
		return smsBusinessError(code)
	}
	return nil
}

// LoginWithSms 用短信验证码换凭证（成功响应带 access_token/refresh_token）。
func (c *Client) LoginWithSms(phone, smsCode string) (*auth.Auth, error) {
	smsCode = strings.TrimSpace(smsCode)
	if smsCode == "" {
		return nil, &Error{Kind: ErrClient, Status: 400, Msg: "请填写短信验证码"}
	}
	enc, err := EncryptPhone(phone)
	if err != nil {
		return nil, &Error{Kind: ErrClient, Status: 400, Msg: err.Error()}
	}
	resp, err := c.doJSON(http.MethodPost, c.authBase()+LoginSmsPath, map[string]string{
		"nation_code": smsNationCode,
		"phone":       enc,
		"sms_code":    smsCode,
	}, c.clientIDHeaders())
	if err != nil {
		return nil, err
	}
	if code, ok := businessCodeOf(resp.Body); ok && code != 0 {
		return nil, smsBusinessError(code)
	}
	data, err := payloadOf(resp, "短信登录")
	if err != nil {
		return nil, err
	}
	access := stripBearerToken(text(data, "access_token", "accessToken"))
	refresh := stripBearerToken(text(data, "refresh_token", "refreshToken"))
	if access == "" {
		return nil, &Error{Kind: ErrServer, Status: 502, Msg: "小浣熊未返回 access_token"}
	}
	creds := &auth.Auth{Data: auth.Data{
		AccessToken:  access,
		RefreshToken: refresh,
		ExpiresAt:    auth.JWTExpiry(access),
		Nickname:     text(data, "office_org_name", "office_identity"),
	}}
	// uid/昵称以 user_info 为准：短信登录响应里的 user_id 可能是平台 id（pu-…），
	// 与 token 导入路径存的 office id 不是同一套，混用会让同一个人在池里占两行。
	if uid, nick, _, uerr := c.GetUserInfo(creds); uerr == nil {
		if uid != "" {
			creds.UID = uid
		}
		if nick != "" {
			creds.Nickname = nick
		}
	}
	if creds.UID == "" {
		return nil, &Error{Kind: ErrServer, Status: 502, Msg: "小浣熊短信登录未返回用户标识"}
	}
	if creds.Nickname == "" {
		creds.Nickname = creds.UID
	}
	return creds, nil
}

// smsBusinessError 把短信链路的业务码翻译成人话（原文附在 Message 里便于排障）。
func smsBusinessError(code int) error {
	switch code {
	case 100002:
		return &Error{Kind: ErrClient, Status: 400, Msg: "手机号格式不正确（或该区号暂不支持）"}
	case 100003:
		return &Error{Kind: ErrServer, Status: 500, Msg: "手机号加密参数被上游拒绝（params_encryted_error）"}
	case 100006:
		return &Error{Kind: ErrClient, Status: 400, Msg: "人机校验未通过：请重新拖动滑块后再发送"}
	}
	return &BusinessError{Code: code, Message: "短信登录被上游拒绝"}
}
