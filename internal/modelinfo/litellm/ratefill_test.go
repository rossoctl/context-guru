package litellm

import (
	"context"
	"github.com/rossoctl/context-guru/internal/modelinfo/ratefilltest"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// Part of issue #856: one function decides a cache price. This test records what each caller
// priced BEFORE the change (fixture made on origin/main c2a06dd5) and fails if any value moves.

func TestRateFillLiteLLM(t *testing.T) {
	build := func(extra string) string {
		s := "{"
		first := true
		for _, m := range ratefilltest.Models {
			if m == "" {
				continue
			}
			if !first {
				s += ","
			}
			first = false
			s += strconv.Quote(m) + `: {"max_input_tokens": 1000, "input_cost_per_token": 5e-06, "output_cost_per_token": 2.5e-05` + extra + `}`
		}
		return s + "}"
	}
	docs := map[string]string{
		"blank":    build(""),
		"stated":   build(`, "cache_read_input_token_cost": 7e-07, "cache_creation_input_token_cost": 9e-06`),
		"readonly": build(`, "cache_read_input_token_cost": 7e-07`),
	}
	got := map[string]string{}
	for name, doc := range docs {
		d := doc
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(d)) }))
		l := NewLiteLLM(srv.URL, srv.Client(), time.Hour)
		ctx := context.Background()
		for i := 0; i < 400; i++ {
			if _, ok := l.Price(ctx, "premium"); ok {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		for _, m := range ratefilltest.Models {
			if m == "" {
				continue
			}
			p, ok := l.Price(ctx, m)
			got["litellm/"+name+"/"+m] = strconv.FormatBool(ok) + " " +
				ratefilltest.F(p.Input, p.Output, p.CacheRead, p.CacheWrite)
		}
		srv.Close()
	}
	ratefilltest.Check(t, got)
}
