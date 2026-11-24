package main

import "testing"

func TestABareWordBeforeASubcommandIsATypo(t *testing.T) {
	for _, c := range []struct {
		args  []string
		stray string
	}{
		{[]string{"ls"}, "ls"},
		{[]string{"-D", "stauts"}, "stauts"},
		{[]string{"up", "-D", "web"}, ""},
		{[]string{"-f", "x.yaml", "-D"}, ""},
		{[]string{"-p", "8099", "process", "list"}, ""},
		{[]string{"--log-file", "pc.log", "attach"}, ""},
		{[]string{"-D"}, ""},
	} {
		if got := parsePC(c.args).stray; got != c.stray {
			t.Errorf("%v: stray %q, want %q", c.args, got, c.stray)
		}
	}
}
