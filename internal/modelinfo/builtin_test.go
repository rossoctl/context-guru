package modelinfo_test

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/internal/modelinfo/litellm"
)

func bundledMap(t *testing.T) []byte {
	t.Helper()
	f, err := os.Open("litellm/testdata/litellm_prices.json.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Every built-in entry must exist in the bundled snapshot with the same window.
func TestBuiltinPinnedToBundledMap(t *testing.T) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(bundledMap(t), &raw); err != nil {
		t.Fatal(err)
	}
	type entry struct {
		Max float64 `json:"max_input_tokens"`
	}
	doc := map[string]entry{}
	for k, v := range raw {
		var e entry
		if json.Unmarshal(v, &e) == nil {
			doc[k] = e
		}
	}
	builtinWindows := modelinfo.DefaultBuiltin().Windows()
	if len(builtinWindows) == 0 {
		t.Fatal("builtin list is empty")
	}
	for id, w := range builtinWindows {
		e, ok := doc[id]
		if !ok {
			t.Errorf("%s: not in the bundled map", id)
			continue
		}
		if int(e.Max) != w {
			t.Errorf("%s: builtin %d, bundled map %d", id, w, int(e.Max))
		}
	}
}

// With no map loaded, each listed id (and its gateway forms) is exact.
func TestBuiltinExactWithoutMap(t *testing.T) {
	unloaded := litellm.NewLiteLLM("http://127.0.0.1:1/none", &http.Client{Timeout: 50 * time.Millisecond}, time.Hour)
	c := modelinfo.NewChain(unloaded)
	ctx := context.Background()
	for id, want := range modelinfo.DefaultBuiltin().Windows() {
		for _, form := range []string{id, "aws/" + id, "anthropic/" + id, id + "[1m]", "aws/" + id + "[1m]"} {
			w, exact, ok := c.WindowExact(ctx, form)
			if !ok || !exact || w != want {
				t.Errorf("%s = %d,exact=%v,ok=%v; want %d,true,true", form, w, exact, ok, want)
			}
		}
	}
}

// An unlisted id, and an id that only shares a prefix with an entry, stay inexact.
func TestBuiltinStaysInexactForUnlisted(t *testing.T) {
	c := modelinfo.NewChain(litellm.NewLiteLLM("http://127.0.0.1:1/none", &http.Client{Timeout: 50 * time.Millisecond}, time.Hour))
	ctx := context.Background()
	for _, id := range []string{
		"claude-sonnet-5-future", "claude-sonnet-5-5-future", "claude-opus-5-1", "claude-opus-4-60",
		"my-claude-opus-5", "claude-sonnet", "claude", "claude-brand-new-9", "gpt-5",
	} {
		if _, exact, _ := c.WindowExact(ctx, id); exact {
			t.Errorf("%s resolved exact; want inexact", id)
		}
		if _, ok := modelinfo.DefaultBuiltin().Window(ctx, id); ok {
			t.Errorf("Builtin answered for %s; want a miss", id)
		}
	}
}

// The downloaded map answers before the built-in list.
func TestBuiltinBehindLoadedMap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"claude-opus-5":{"max_input_tokens":64000}}`))
	}))
	defer srv.Close()
	l := litellm.NewLiteLLM(srv.URL, srv.Client(), time.Hour)
	waitForBuiltinTest(t, l, "claude-opus-5")
	w, exact, ok := modelinfo.NewChain(l).WindowExact(context.Background(), "claude-opus-5")
	if !ok || !exact || w != 64000 {
		t.Errorf("got %d,%v,%v; want the map's 64000,true,true", w, exact, ok)
	}
}

func waitForBuiltinTest(t *testing.T, l *litellm.LiteLLM, model string) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if _, ok := l.Window(context.Background(), model); ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("litellm map never loaded (%s never resolved)", model)
}
