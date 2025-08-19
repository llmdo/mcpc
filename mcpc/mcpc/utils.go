package mcpc

import (
	"bytes"
	"encoding/json"
	"os"
)

/* =========================
   工具函数
   ========================= */

func mustJSON(v any) []byte {
	bs, _ := json.Marshal(v)
	return bs
}

func isJSONArray(b []byte) bool {
	b = bytes.TrimSpace(b)
	return len(b) > 0 && b[0] == '['
}

/**
 ** 对外作为封装使用环境变量的函数
 */
func EnvStr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
