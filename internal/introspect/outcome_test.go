package introspect

import "testing"

func TestLooksLikeHelp(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		{
			name: "real git bogus -h error message is a single line",
			text: "git: 'bogus' is not a git command. See 'git --help'.\n",
			want: false,
		},
		{
			name: "empty text",
			text: "",
			want: false,
		},
		{
			name: "blank lines only",
			text: "\n\n\n",
			want: false,
		},
		{
			name: "a genuine two-line help fragment with a flag definition line",
			text: "usage: git log [<options>]\n\n    -q, --quiet   suppress diff output\n",
			want: true,
		},
		{
			// Ruling 2's own worked counter-example: a bare line-count bound
			// (">1 non-blank line") cannot tell this apart from real help —
			// both clear it identically. Positive structure must reject it.
			name: "a two-line CLI error page must not look like help",
			text: "error: pathspec 'xyz' did not match any file(s) known to git\nDid you forget to 'git add'?\n",
			want: false,
		},
		{
			name: "a subcommand catalog listing with no flags at all",
			text: "usage: fj <command> [<args>]\n\nAvailable commands:\n  pr        manage pull requests\n  issue     manage issues\n",
			want: true,
		},
		{
			// go build -h's real shape: a usage line, a blank line, then a
			// further section — no flag definition line, no indented
			// two-column block, but still a genuine (if terse) help page.
			name: "a usage line followed by a further section, no flags",
			text: "usage: go build [-o output] [build flags] [packages]\n\nUse 'go help build' for more information.\n",
			want: true,
		},
		{
			name: "a usage line with nothing after it is not enough on its own",
			text: "usage: foo\n",
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := looksLikeHelp(c.text); got != c.want {
				t.Errorf("looksLikeHelp(%q) = %v, want %v", c.text, got, c.want)
			}
		})
	}
}

func TestFlagCoverage(t *testing.T) {
	// Every flag-shaped token — both the long AND short form of each flag,
	// since "-a, --append" contains both — was captured by the winning
	// parse.
	text := "Usage: widget [OPTION]...\n\n  -a, --append   append\n  -v, --verbose  explain\n"
	flags := map[string]Flag{"--append": {Short: "-a"}, "--verbose": {Short: "-v"}}
	if found, total := flagCoverage(text, flags); found != 4 || total != 4 {
		t.Errorf("found/total = %d/%d, want 4/4 (long AND short forms of both flags captured)", found, total)
	}

	// Half the flags were captured (both their long and short tokens) — the
	// other half's long AND short tokens are both misses.
	flagsHalf := map[string]Flag{"--append": {Short: "-a"}}
	if found, total := flagCoverage(text, flagsHalf); found != 2 || total != 4 {
		t.Errorf("found/total = %d/%d, want 2/4", found, total)
	}

	// No flag-shaped tokens anywhere: nothing to miss, but also nothing to
	// verify against — UNMEASURABLE (total == 0), not a silent "1.0" that
	// would be indistinguishable from a genuinely verified zero-flag result.
	if found, total := flagCoverage("Usage: widget\n\nNo options.\n", map[string]Flag{}); total != 0 || found != 0 {
		t.Errorf("found/total of a flagless help = %d/%d, want 0/0 (unmeasurable)", found, total)
	}

	// git's bracket [no-]flag syntax must be VISIBLE to the coverage
	// denominator now (Ruling 1's fix) — the previous fallback-regex-based
	// denominator was blind to it on both sides, which is what let
	// `git log -h` score a false 1.0 while missing real flags it simply
	// couldn't see. Here it must be seen AND correctly matched, for the
	// right reason (found, not invisible).
	gitStyle := "    --[no-]amend          amend previous commit\n"
	if found, total := flagCoverage(gitStyle, map[string]Flag{"--amend": {}}); found != 1 || total != 1 {
		t.Errorf("found/total = %d/%d, want 1/1 (--[no-]amend normalizes to --amend and is found)", found, total)
	}

	// A short-only flag (no long form at all, e.g. git log -h's own
	// "-L <range:file>") that no parser captures under ANY key must show up
	// as a genuine miss, not be silently invisible to the denominator the
	// way the old fallback-regex-only scan made it.
	shortOnly := "    -L <range:file>       trace the evolution of line range\n"
	if found, total := flagCoverage(shortOnly, map[string]Flag{}); total != 1 || found != 0 {
		t.Errorf("found/total = %d/%d, want 0/1 (a short-only flag the parse never captured)", found, total)
	}

	// A short flag IS found when some captured Flag carries it as Short,
	// even though Flags is keyed by long name — both the short AND long
	// tokens present in the text are accounted for.
	withShort := map[string]Flag{"--quiet": {Short: "-q"}}
	if found, total := flagCoverage("    -q, --quiet   be quiet\n", withShort); found != 2 || total != 2 {
		t.Errorf("found/total = %d/%d, want 2/2 (short flag matched via Flag.Short, long flag matched via key)", found, total)
	}
}

// TestFlagCoverage_AgainstGitLogFixture is the design's own worked example
// end to end: `git log -h`'s real output (testdata/git_log_h.txt) contains a
// short-only flag (-L) that GitParser's own regexes never capture under any
// key (all three require a long "--" form). Coverage must show that gap
// rather than reporting full confidence for a parse that missed it.
func TestFlagCoverage_AgainstGitLogFixture(t *testing.T) {
	helpText := readTestData(t, "git_log_h.txt")
	result := BestParse(helpText)
	found, total := flagCoverage(helpText, result.Subcommand.Flags)
	if total == 0 {
		t.Fatal("test fixture bug: git_log_h.txt should contain measurable flag-shaped tokens")
	}
	if found >= total {
		t.Errorf("found/total = %d/%d, want found < total — git_log_h.txt's -L short-only flag is never captured by any GitParser regex (all three require a long form)", found, total)
	}
}

func TestFlagShapeTokens(t *testing.T) {
	cases := []struct {
		name string
		text string
		want []string
	}{
		{"long flag", "  --output <file>   write here\n", []string{"--output"}},
		{"no-prefix long flag normalizes", "    --[no-]amend          amend previous commit\n", []string{"--amend"}},
		{"equals-form long flag", "  --level=LEVEL   set level\n", []string{"--level"}},
		{"short flag alone", "  -L <range:file>  trace\n", []string{"-L"}},
		{"short and long together", "  -q, --quiet   be quiet\n", []string{"-q", "--quiet"}},
		{"prose mention of --help is still a token (broad recall)", "See 'git --help'.\n", []string{"--help"}},
		{"embedded hyphenated word is not a short flag", "    --[no-]decorate-refs <pattern>\n", []string{"--decorate-refs"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := flagShapeTokens(c.text)
			gotSet := map[string]bool{}
			for _, tok := range got {
				gotSet[tok] = true
			}
			if len(gotSet) != len(c.want) {
				t.Fatalf("flagShapeTokens(%q) = %v, want %v", c.text, got, c.want)
			}
			for _, w := range c.want {
				if !gotSet[w] {
					t.Errorf("flagShapeTokens(%q) missing %q, got %v", c.text, w, got)
				}
			}
		})
	}
}
