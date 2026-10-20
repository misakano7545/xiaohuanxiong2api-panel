package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// maxBodyBytes 请求体上限（8MB）。三套入站协议共用同一口限。
const maxBodyBytes = 8 << 20

// errBodyTooLarge 请求体超过上限。
var errBodyTooLarge = errors.New("request body too large")

// readCappedBody 读请求体，读到上限就停（先限长再落内存，避免持 key 打爆内存）。
func readCappedBody(r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBodyBytes {
		return nil, errBodyTooLarge
	}
	return body, nil
}

// writeJSON 输出 JSON。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeOpenAIError 输出 OpenAI 形状的错误信封。三套入站协议的内层都用它，
// 外层的 messages/responses 写手再翻成 Anthropic / Responses 的形状。
func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "invalid_request_error",
			"code":    code,
		},
	})
}
