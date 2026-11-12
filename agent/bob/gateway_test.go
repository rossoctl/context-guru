package bob

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/agent"
)

func TestBobIsAnOpenAICatchAllGateway(t *testing.T) {
	want := agent.GatewaySpec{Dialect: "openai", CatchAll: true}
	if got := (Bob{}).Gateway(); got != want {
		t.Fatalf("Gateway() = %+v, want %+v", got, want)
	}
}

// The rule the proxy had before the move: the profile always, model/info only when not hosted.
func TestWantsReplyOnlyForTheProfileAndLocalModelInfo(t *testing.T) {
	for _, c := range []struct {
		call agent.ControlCall
		want bool
	}{
		{agent.ControlCall{Method: http.MethodGet, Path: ProfilePath}, true},
		{agent.ControlCall{Method: http.MethodGet, Path: ProfilePath, Hosted: true}, true},
		{agent.ControlCall{Method: http.MethodGet, Path: "/ADMIN/v1/Profile"}, true},
		{agent.ControlCall{Method: http.MethodGet, Path: "/inference/v1/model/info"}, true},
		{agent.ControlCall{Method: http.MethodGet, Path: "/v1/model/info"}, true},
		{agent.ControlCall{Method: http.MethodGet, Path: "/inference/v1/model/info", Hosted: true}, false},
		{agent.ControlCall{Method: http.MethodGet, Path: "/v1/model/info", Hosted: true}, false},
		{agent.ControlCall{Method: http.MethodPost, Path: ProfilePath}, false},
		{agent.ControlCall{Method: http.MethodPost, Path: "/inference/v1/model/info"}, false},
		{agent.ControlCall{Method: http.MethodGet, Path: "/inference/v1/models"}, false},
		{agent.ControlCall{Method: http.MethodPost, Path: ModelPath}, false},
		{agent.ControlCall{Method: http.MethodGet, Path: "/"}, false},
	} {
		if got := (Bob{}).WantsReply(c.call); got != c.want {
			t.Errorf("WantsReply(%+v) = %v, want %v", c.call, got, c.want)
		}
	}
}

func TestReplyStripsTheProfileRegion(t *testing.T) {
	in := []byte(`{"user_id":"u","instances":[{"instance_id":"i1","region":"us-east","region_domain":"us-east.example.com"}]}`)
	want, ok := StripProfileRegion(in)
	if !ok {
		t.Fatal("StripProfileRegion refused the test profile")
	}

	got := (Bob{}).Reply(agent.ControlCall{Method: http.MethodGet, Path: ProfilePath}, in)

	if string(got.Body) != string(want) || strings.Contains(string(got.Body), "region") {
		t.Fatalf("Reply.Body = %s, want %s", got.Body, want)
	}
	if got.Windows != nil {
		t.Errorf("Reply.Windows = %v, want nil for a profile", got.Windows)
	}
}

// A body that is not a profile gives the zero reply: the host sends it unchanged.
func TestReplyToAnUnreadableProfileIsTheZeroReply(t *testing.T) {
	got := (Bob{}).Reply(agent.ControlCall{Method: http.MethodGet, Path: ProfilePath}, []byte("not json"))

	if !reflect.DeepEqual(got, agent.ControlReply{}) {
		t.Fatalf("Reply = %+v, want the zero reply", got)
	}
}

func TestReplyToModelInfoGivesTheWindowsAndNoBody(t *testing.T) {
	in := []byte(`{"data":[{"model_name":"m1","model_info":{"max_input_tokens":200000}},{"model_name":"m2","model_info":{}}]}`)

	got := (Bob{}).Reply(agent.ControlCall{Method: http.MethodGet, Path: "/inference/v1/model/info"}, in)

	if want := map[string]int{"m1": 200000}; !reflect.DeepEqual(got.Windows, want) {
		t.Errorf("Reply.Windows = %v, want %v", got.Windows, want)
	}
	if got.Body != nil {
		t.Errorf("Reply.Body = %q, want nil (model/info is sent unchanged)", got.Body)
	}
}

// Reply reads only the calls WantsReply asks for: another call gives the zero reply.
func TestReplyToAnotherCallIsTheZeroReply(t *testing.T) {
	in := []byte(`{"data":[{"model_name":"m1","model_info":{"max_input_tokens":200000}}]}`)
	for _, c := range []agent.ControlCall{
		{Method: http.MethodGet, Path: "/inference/v1/models"},
		{Method: http.MethodPost, Path: ProfilePath},
		{Method: http.MethodGet, Path: "/inference/v1/model/info", Hosted: true},
	} {
		if got := (Bob{}).Reply(c, in); !reflect.DeepEqual(got, agent.ControlReply{}) {
			t.Errorf("Reply(%+v) = %+v, want the zero reply", c, got)
		}
	}
}
