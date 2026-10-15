package cheapmodel

import (
	"context"
	"net/http"
	"net/http/httptest"
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

func TestInsertAskMessagePreservesStoredPrefix(t *testing.T) {
	body := `{"model":"m","thinking":{"type":"enabled","budget_tokens":1024},"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral","ttl":"1h"}}],"tools":[{"name":"t"}],"messages":[{"role":"user","content":[{"type":"text","text":"a"}]},{"role":"assistant","content":[{"type":"text","text":"b"}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"c","cache_control":{"type":"ephemeral"}}]}]}`
	for _, role := range []string{"user", "system"} {
		out, err := insertAskMessageAs([]byte(body), "ask", role)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{"model", "thinking", "system", "tools"} {
			if gjson.Get(body, f).Raw != gjson.GetBytes(out, f).Raw {
				t.Errorf("%s: field %s changed", role, f)
			}
		}
		in, o := gjson.Get(body, "messages").Array(), gjson.GetBytes(out, "messages").Array()
		if len(o) != len(in)+1 {
			t.Fatalf("%s: want one appended message, got %d -> %d", role, len(in), len(o))
		}
		for i := range in {
			if in[i].Raw != o[i].Raw {
				t.Errorf("%s: stored message %d changed", role, i)
			}
		}
		if o[len(o)-1].Raw == "" || gjson.Get(o[len(o)-1].Raw, "cache_control").Exists() {
			t.Errorf("%s: appended message must carry no cache_control", role)
		}
	}
}

func TestCompletePrefixedForwardsTheSessionBeta(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("anthropic-beta"))
		w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()
	a := Anthropic{BaseURL: srv.URL, APIKey: "k", Model: "m"}
	body := []byte(`{"messages":[{"role":"user","content":"a"}]}`)
	if _, _, err := a.CompletePrefixedAs(WithBeta(context.Background(), "context-1m-2025-08-07,x"), body, "ask", "user"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.CompletePrefixed(context.Background(), body, "ask"); err != nil {
		t.Fatal(err)
	}
	if got[0] != "context-1m-2025-08-07,x" || got[1] != "" {
		t.Errorf("beta header = %q, want the session's value exactly, and none when the session had none", got)
	}
}
