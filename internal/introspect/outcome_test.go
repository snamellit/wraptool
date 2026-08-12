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
			name: "a genuine two-line help fragment",
			text: "usage: git log [<options>]\n\n    -q, --quiet   suppress diff output\n",
			want: true,
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
	// Every fallback-visible flag token was captured by the winning parse.
	text := "Usage: widget [OPTION]...\n\n  -a, --append   append\n  -v, --verbose  explain\n"
	flags := map[string]Flag{"--append": {}, "--verbose": {}}
	if got := flagCoverage(text, flags); got != 1.0 {
		t.Errorf("coverage = %v, want 1.0 (both tokens captured)", got)
	}

	// Half the fallback-visible tokens were captured.
	flagsHalf := map[string]Flag{"--append": {}}
	if got := flagCoverage(text, flagsHalf); got != 0.5 {
		t.Errorf("coverage = %v, want 0.5", got)
	}

	// No flag-shaped tokens anywhere: nothing to miss, so coverage is 1.0 —
	// combined with ParseOutcomeParsed this is the genuine "no flags" state.
	if got := flagCoverage("Usage: widget\n\nNo options.\n", map[string]Flag{}); got != 1.0 {
		t.Errorf("coverage of a flagless help = %v, want 1.0", got)
	}

	// git's bracket [no-]flag syntax is invisible to the fallback regex on
	// both sides (numerator and denominator) — it must not silently deflate
	// coverage for a genuinely well-parsed git-style flag it can't see at
	// all, since it never enters the "seen" set either.
	gitStyle := "    --[no-]amend          amend previous commit\n"
	if got := flagCoverage(gitStyle, map[string]Flag{"--amend": {}}); got != 1.0 {
		t.Errorf("coverage = %v, want 1.0 (fallback regex can't see [no-] flags at all, so denominator is 0)", got)
	}
}
