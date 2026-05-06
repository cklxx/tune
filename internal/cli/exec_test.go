package cli

import "testing"

func TestShQuote(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "''"},
		{"plain", "plain"},
		{"hello world", "'hello world'"},
		{"it's", `'it'\''s'`},
		{"a|b", "'a|b'"},
		{"a$b", "'a$b'"},
	}
	for _, c := range cases {
		if got := shQuote(c.in); got != c.want {
			t.Errorf("shQuote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestJoinShell(t *testing.T) {
	if got := joinShell([]string{"ls -la /tmp"}); got != "ls -la /tmp" {
		t.Errorf("single arg passthrough lost: %q", got)
	}
	if got := joinShell([]string{"echo", "hello world"}); got != "echo 'hello world'" {
		t.Errorf("multi-arg quoting wrong: %q", got)
	}
}

func TestEnvEscape(t *testing.T) {
	if got := envEscape("FOO=bar baz"); got != "FOO='bar baz'" {
		t.Errorf("envEscape: %q", got)
	}
	if got := envEscape("FOO=plain"); got != "FOO=plain" {
		t.Errorf("envEscape plain: %q", got)
	}
	if got := envEscape("noequals"); got != "noequals" {
		t.Errorf("envEscape no eq: %q", got)
	}
}
