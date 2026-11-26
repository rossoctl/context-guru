package schema

import (
	"reflect"
	"testing"
)

func sseAll(body string) []string {
	var got []string
	SSEData([]byte(body), func(d []byte) bool {
		got = append(got, string(d))
		return true
	})
	return got
}

func TestSSEData(t *testing.T) {
	for _, c := range []struct {
		name, body string
		want       []string
	}{
		{"one event", "data: {\"a\":1}\n\n", []string{`{"a":1}`}},
		{"no space after the colon", "data:{\"a\":1}\n\n", []string{`{"a":1}`}},
		{"CRLF framing", "data: {\"a\":1}\r\n\r\ndata: {\"a\":2}\r\n\r\n", []string{`{"a":1}`, `{"a":2}`}},
		{"the [DONE] marker is skipped", "data: {\"a\":1}\n\ndata: [DONE]\n\n", []string{`{"a":1}`}},
		{"[DONE] with CRLF is skipped", "data: [DONE]\r\n\r\n", nil},
		{"event, id, retry and comment lines are skipped",
			": ping\nevent: message_start\nid: 7\nretry: 5\ndata: {\"a\":1}\n\n", []string{`{"a":1}`}},
		{"an event with no data calls nothing", "event: ping\n\n", nil},
		{"an empty data line calls nothing", "data:\n\ndata: \n\n", nil},
		{"several data lines of one event are joined", "data: {\"a\":\ndata: 1}\n\n", []string{"{\"a\":\n1}"}},
		{"three data lines", "data: x\ndata: y\ndata: z\n\n", []string{"x\ny\nz"}},
		{"whole JSON lines with no blank line between are separate events", "data: {\"a\":1}\ndata: {\"a\":2}\ndata: [DONE]\n", []string{`{"a":1}`, `{"a":2}`}},
		{"data lines of two events are not joined", "data: x\n\ndata: y\n\n", []string{"x", "y"}},
		{"the last event needs no blank line", "data: x\n\ndata: y", []string{"x", "y"}},
		{"a cut line is read as it is", "data: {\"a\":1}\n\ndata: {\"b", []string{`{"a":1}`, `{"b`}},
		{"an empty body", "", nil},
	} {
		if got := sseAll(c.body); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestSSEDataStopsWhenAsked(t *testing.T) {
	n := 0
	SSEData([]byte("data: a\n\ndata: b\n\ndata: c\n\n"), func([]byte) bool {
		n++
		return n < 2
	})
	if n != 2 {
		t.Errorf("fn ran %d times, want 2", n)
	}
}

func TestSSEEventData(t *testing.T) {
	for ev, want := range map[string]string{
		"event: x\ndata: {\"a\":1}\n\n":     `{"a":1}`,
		"data: {\"a\":\r\ndata: 1}\r\n\r\n": "{\"a\":\n1}",
		"data: [DONE]\n\n":                  "",
		"event: ping\n\n":                   "",
	} {
		if got := string(SSEEventData([]byte(ev))); got != want {
			t.Errorf("%q: got %q, want %q", ev, got, want)
		}
	}
}

func TestSSEDataLine(t *testing.T) {
	if v, ok := SSEDataLine([]byte("data: {\"a\":1}\r")); !ok || string(v) != `{"a":1}` {
		t.Errorf("got %q %v", v, ok)
	}
	if _, ok := SSEDataLine([]byte("event: x")); ok {
		t.Error("an event line is not a data line")
	}
}
