package cheapmodel

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestInsertAskMessageAsSystem(t *testing.T) {
	roles := func(b []byte) string {
		s := ""
		for _, m := range gjson.GetBytes(b, "messages").Array() {
			s += m.Get("role").String()[:1]
		}
		return s
	}
	for _, tc := range []struct{ name, body, role, want string }{
		{"system after user", `{"messages":[{"role":"user","content":"a"}]}`, "system", "us"},
		{"user is the default", `{"messages":[{"role":"user","content":"a"}]}`, "user", "uu"},
		{"system after assistant falls back to user", `{"messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"}]}`, "system", "uau"},
		{"system after system keeps the existing handling", `{"messages":[{"role":"user","content":"a"},{"role":"system","content":"r"}]}`, "system", "usau"},
	} {
		out, err := insertAskMessageAs([]byte(tc.body), "ask", tc.role)
		if err != nil {
			t.Fatal(err)
		}
		if got := roles(out); got != tc.want {
			t.Errorf("%s: roles %q, want %q", tc.name, got, tc.want)
		}
		if last := func() string {
			a := gjson.GetBytes(out, "messages").Array()
			return a[len(a)-1].Get("content").String()
		}(); last != "ask" {
			t.Errorf("%s: the ask must be last, got %q", tc.name, last)
		}
	}
}
