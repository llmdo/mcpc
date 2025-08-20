package ssews

import (
	"errors"
	"net/http"
	"strings"
)

/* =========================
   鉴权
   ========================= */

func extractClientIDFromAuth(r *http.Request) (string, error) {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return "", errors.New("missing Authorization")
	}
	parts := strings.SplitN(auth, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return "", errors.New("invalid Authorization header")
	}
	return parts[1], nil
}

/* =========================
   工具函数
   ========================= */

func safeClose[T any](ch chan T) {
	defer func() { _ = recover() }()
	close(ch)
}
