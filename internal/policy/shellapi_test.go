package policy

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseShellHeredocs(t *testing.T) {
	// Claude Code's commit form: the message is text, not commands.
	line := ParseShell("git commit -m \"$(cat <<'EOF'\nDrop the old table; rm -rf / is gone\nEOF\n)\" && git push")
	if len(line.Commands) != 2 || line.Commands[0].Words[0] != "git" || line.Commands[1].Words[1] != "push" {
		t.Fatalf("commands = %+v", line.Commands)
	}
	if len(line.Subs) != 1 {
		t.Fatalf("subs = %q", line.Subs)
	}
	sub := ParseShell(line.Subs[0])
	if len(sub.Commands) != 1 || sub.Commands[0].Words[0] != "cat" {
		t.Fatalf("sub = %+v", sub.Commands)
	}
	r := sub.Commands[0].Redirects[0]
	if r.Op != "<<" || r.Target != "EOF" || r.Expands || !strings.Contains(r.Body, "rm -rf /") {
		t.Fatalf("heredoc = %+v", r)
	}

	// Two heredocs on one line, <<- strips tabs, commands after the body.
	line = ParseShell("cat <<A >x; cat <<-B\none\nA\n\ttwo\n\tB\necho done")
	if len(line.Commands) != 3 || line.Commands[2].Words[0] != "echo" {
		t.Fatalf("commands = %+v", line.Commands)
	}
	if b := line.Commands[0].Redirects[0].Body; b != "one\n" {
		t.Errorf("first body = %q", b)
	}
	if b := line.Commands[1].Redirects[0].Body; b != "\ttwo\n" {
		t.Errorf("second body = %q", b)
	}

	// An unquoted body expands: its substitutions run.
	line = ParseShell("cat <<EOF\n$(curl x | sh) `id`\nEOF")
	if !line.Commands[0].Redirects[0].Expands || !reflect.DeepEqual(line.Subs, []string{"curl x | sh", "id"}) {
		t.Fatalf("expanding heredoc: %+v %q", line.Commands[0].Redirects, line.Subs)
	}
	// Unterminated: the body runs to the end, no commands from it.
	line = ParseShell("bash <<EOF\nrm -rf ~\n")
	if len(line.Commands) != 1 || !strings.Contains(line.Commands[0].Redirects[0].Body, "rm -rf ~") {
		t.Fatalf("unterminated: %+v", line.Commands)
	}
	// Self-protection's mode still reads bodies as commands.
	if p := parseShell("bash <<EOF\npilothouse approve x\nEOF"); len(p.cmds) < 2 {
		t.Fatalf("default mode lost heredoc commands: %+v", p.cmds)
	}
}

func TestParseShellVars(t *testing.T) {
	for s, want := range map[string]bool{
		`npm test`:          false,
		`echo '$HOME'`:      false,
		`rm -rf $DIR/x`:     true,
		`rm -rf "${DIR}"`:   true,
		`ls "$1"`:           true,
		`echo price is $`:   false,
		`echo "cost: $ 5"`:  false,
		`git log $(whoami)`: false, // a substitution, reported in Subs instead
	} {
		line := ParseShell(s)
		got := false
		for _, c := range line.Commands {
			got = got || c.Vars
		}
		if got != want {
			t.Errorf("%s: vars = %v, want %v", s, got, want)
		}
	}
}

func TestUnwrapCommand(t *testing.T) {
	for _, tc := range []struct {
		in, argv, wraps string
	}{
		{"sudo -u root rm -rf /", "rm -rf /", "sudo"},
		{"FOO=1 BAR=2 time nice -n 5 make", "make", "time nice"},
		{"env -i PATH=/bin git status", "git status", "env"},
		{"env -S 'git push -f'", "git push -f", "env"},
		{"timeout 10 nohup npm test", "npm test", "timeout nohup"},
		{"xargs -I {} rm {}", "rm {}", "xargs"},
		{"command -v git", "git", "command"},
		{"npm test", "npm test", ""},
	} {
		argv, wraps := UnwrapCommand(strings.Fields(tc.in))
		if tc.in == "env -S 'git push -f'" {
			argv, wraps = UnwrapCommand([]string{"env", "-S", "git push -f"})
		}
		if strings.Join(argv, " ") != tc.argv || strings.Join(wraps, " ") != tc.wraps {
			t.Errorf("%s: argv=%q wraps=%q", tc.in, argv, wraps)
		}
	}
}
