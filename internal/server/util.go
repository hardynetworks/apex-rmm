package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// fail maps an error to an HTTP response.
func fail(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	var he *httpError
	if errors.As(err, &he) {
		writeErr(w, he.status, he.msg)
		return
	}
	slog.Error("request failed", "err", err)
	writeErr(w, http.StatusInternalServerError, "internal error")
}

type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }

func badRequest(msg string) error { return &httpError{http.StatusBadRequest, msg} }
func conflict(msg string) error   { return &httpError{http.StatusConflict, msg} }

func decode(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 4<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return badRequest("invalid JSON: " + err.Error())
	}
	return nil
}

func randToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// randPassword returns an alphanumeric password (for RustDesk).
func randPassword(n int) string {
	const chars = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnpqrstuvwxyz23456789"
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = chars[int(b[i])%len(chars)]
	}
	return string(b)
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func constEq(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if v := r.Header.Get("CF-Connecting-IP"); v != "" {
			return v
		}
		if v := r.Header.Get("X-Real-IP"); v != "" {
			return v
		}
		if v := r.Header.Get("X-Forwarded-For"); v != "" {
			return strings.TrimSpace(strings.Split(v, ",")[0])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func nz(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if strings.EqualFold(x, v) {
			return true
		}
	}
	return false
}
