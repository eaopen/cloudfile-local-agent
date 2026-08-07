package config

import "testing"

func TestResolveOpenRulePrefersFirstSpecificMatch(t *testing.T) {
	config := Config{OpenRules: []OpenRule{
		{ID: "cad-view", Modes: []string{"local-view"}, Extensions: []string{"dwg"}, Command: []string{"/Applications/CAD.app", "{file}"}},
		{ID: "fallback", Modes: []string{"local-view"}, Extensions: []string{"*"}, Command: []string{"/Applications/Preview.app", "{file}"}},
	}}
	rule, ok := config.ResolveOpenRule("local-view", "drawing.DWG")
	if !ok || rule.ID != "cad-view" {
		t.Fatalf("got %#v, %v", rule, ok)
	}
}

func TestOpenRuleRejectsAmbiguousOrUnsafeCommand(t *testing.T) {
	rule := OpenRule{ID: "bad", Modes: []string{"local-edit"}, Extensions: []string{"dwg"}, Command: []string{"editor", "{file}"}}
	if err := rule.Validate(); err == nil {
		t.Fatal("relative executable must be rejected")
	}
	rule.Command = []string{"/Applications/CAD.app", "{file}", "{file}"}
	if err := rule.Validate(); err == nil {
		t.Fatal("multiple file placeholders must be rejected")
	}
}
