// Command breaking-footer rejects a breaking-change footer the subject never
// declared. It is a template vendored into a target repository, where the
// commitlint workflow runs it over a pull request's commit range; that
// repository may have no Go module of its own, so this is one standalone
// stdlib-only file that `go run` accepts by name. The rule it enforces is
// owned by github-conventions, references/commits.md.
//
// conventional-commits-parser treats any line beginning with BREAKING CHANGE
// or BREAKING-CHANGE, followed by a colon or whitespace, as a footer — with no
// regard for whether it is prose. Body text that merely wraps the keyword onto
// the start of a line makes semantic-release cut a major release, silently.
// Requiring the subject to carry "!" as well makes the intent explicit, so
// accidental prose no longer looks like a deliberate break.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

var (
	footerLine   = regexp.MustCompile(`^[[:space:]*]*BREAKING[ -]CHANGE([[:space:]:]|$)`)
	declaredHead = regexp.MustCompile(`^[a-zA-Z]+(\([^)]*\))?!:`)
)

const guidance = `
Either declare the break in the subject (feat!: ...), or reword the body so the
keyword does not begin a line. semantic-release reads it as a footer either
way, and a major release cannot be undone without rewriting history.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr *os.File) int {
	if len(args) != 2 {
		fmt.Fprintln(stderr, "usage: breaking-footer <base-sha> <head-sha>")
		return 2
	}

	// Merge commits carry generated bodies nobody wrote, and the preset drops them.
	out, err := git("rev-list", "--no-merges", args[0]+".."+args[1])
	if err != nil {
		fmt.Fprintln(stderr, "breaking-footer:", err)
		return 2
	}

	commits := lines(out)
	offenders := 0
	for _, sha := range commits {
		body, err := git("log", "-1", "--format=%b", sha)
		if err != nil {
			fmt.Fprintln(stderr, "breaking-footer:", err)
			return 2
		}
		offending := offendingLines(body)
		if len(offending) == 0 {
			continue
		}

		subject, err := git("log", "-1", "--format=%s", sha)
		if err != nil {
			fmt.Fprintln(stderr, "breaking-footer:", err)
			return 2
		}
		if declaredHead.MatchString(subject) {
			continue
		}

		fmt.Fprintf(stdout, "%s declares no break in its subject but its body starts a line with the keyword:\n", sha[:8])
		fmt.Fprintf(stdout, "  subject: %s\n", subject)
		for _, l := range offending {
			fmt.Fprintf(stdout, "  body line %d:%s\n", l.number, l.text)
		}
		offenders++
	}

	if offenders != 0 {
		fmt.Fprint(stderr, guidance)
		return 1
	}

	fmt.Fprintf(stdout, "checked %d commit(s): no undeclared breaking-change footers\n", len(commits))

	return 0
}

type bodyLine struct {
	number int
	text   string
}

func offendingLines(body string) []bodyLine {
	var offending []bodyLine
	for i, text := range strings.Split(body, "\n") {
		if footerLine.MatchString(text) {
			offending = append(offending, bodyLine{number: i + 1, text: text})
		}
	}

	return offending
}

func git(args ...string) (string, error) {
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}

	return strings.TrimRight(string(out), "\n"), nil
}

func lines(out string) []string {
	if out == "" {
		return nil
	}

	return strings.Split(out, "\n")
}
