package server

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestRustDeskConfigString(t *testing.T) {
	s := rustDeskConfigString("rd.example.com", "rd.example.com", "KEY=")
	r := []rune(s)
	for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
		r[i], r[j] = r[j], r[i]
	}
	b, err := base64.RawURLEncoding.DecodeString(string(r))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil || m["host"] != "rd.example.com" || m["key"] != "KEY=" {
		t.Fatalf("bad config %s", b)
	}
}

func TestSafeReturn(t *testing.T) {
	for in, want := range map[string]string{"/devices": "/devices", "//evil.com": "/", "https://x": "/", "": "/", `/\evil`: "/"} {
		if got := safeReturn(in); got != want {
			t.Errorf("safeReturn(%q)=%q want %q", in, got, want)
		}
	}
}

func TestRoles(t *testing.T) {
	if roleRank("admin") <= roleRank("technician") || roleRank("viewer") <= roleRank("") {
		t.Fatal("role ordering")
	}
}
