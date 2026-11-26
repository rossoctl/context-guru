package tooldefer

import (
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/components/reformat"
	"github.com/rossoctl/context-guru/config"
)

// The question "is deferral on?" has one answer: toolfilter's `defer` value is `auto`
// (reformat.DeferMode). PipeOn reads it off a built pipeline. DocOn reads it off a configuration
// document, for a caller that has no built pipeline (the start-up scan of tenants, the save-more
// tools rows).

// PipeOn reports whether pipe's toolfilter asks for deferral, with its keep list and its
// always-defer list (#878).
func PipeOn(pipe *components.Pipeline) (on bool, keep, always []string) {
	if pipe == nil {
		return false, nil, nil
	}
	tf, ok := pipe.Find("toolfilter").(interface {
		Defers() bool
		Keep() []string
		AlwaysDefer() []string
	})
	if !ok || !tf.Defers() {
		return false, nil, nil
	}
	return true, tf.Keep(), tf.AlwaysDefer()
}

// DocOn reports whether a configuration document turns tool deferral on: its pipeline holds
// toolfilter with `defer: auto` (the ultrahigh preset, or a document that sets it).
func DocOn(doc string) bool {
	c, err := config.LoadBytes([]byte(doc))
	if err != nil {
		return false
	}
	has := false
	for _, n := range c.Pipeline {
		has = has || n == "toolfilter"
	}
	node, ok := c.Components["toolfilter"]
	if !has || !ok {
		return false
	}
	var tf struct {
		Defer string `yaml:"defer"`
	}
	if node.Decode(&tf) != nil {
		return false
	}
	on, err := reformat.DeferMode(tf.Defer)
	return err == nil && on
}
