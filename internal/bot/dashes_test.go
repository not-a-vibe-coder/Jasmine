package bot

import "testing"

func TestReplaceDashPauses(t *testing.T) {
	cases := map[string]string{
		"i missed you — how was the trip":      "i missed you, how was the trip",
		"honestly–that stings":                 "honestly, that stings",
		"it went well - better than i thought": "it went well, better than i thought",
		"- first item\n- second item":          "- first item\n- second item",
		"5 - 3 is 2":                           "5 - 3 is 2",
		"run ```ls - la``` now":                "run ```ls - la``` now",
		"well-known self-hosted thing":         "well-known self-hosted thing",
	}
	for in, want := range cases {
		if got := replaceDashPauses(in); got != want {
			t.Errorf("replaceDashPauses(%q) = %q, want %q", in, got, want)
		}
	}
}
