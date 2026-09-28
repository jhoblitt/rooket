// Package lockowner keeps the record a rooket lock file holds of its holder:
// the rooket that takes the flock writes "<pid> <argv>" into the file, and one
// that finds the lock taken reads it back to name the holder it is waiting on,
// or is refused by.
// The record is diagnostic only. The flock is what excludes, so a record that
// fails to write, or reads back torn, costs a clearer message and nothing else.
package lockowner

import (
	"fmt"
	"os"
	"strings"
)

// Write records this process in the lock file f, whose flock it has just
// taken, replacing whatever an earlier holder left there.
func Write(f *os.File) {
	if err := f.Truncate(0); err != nil {
		return
	}
	if _, err := f.Seek(0, 0); err != nil {
		return
	}
	fmt.Fprintf(f, "%d %s\n", os.Getpid(), strings.Join(os.Args, " "))
}

// At renders the holder recorded in the lock file at path as " (pid <pid>:
// <argv>)", its leading space included so a message can splice it straight
// after a word, or "" when there is nothing trustworthy to report. Reading
// needs no lock and races the holder's own write, so anything unexpected
// yields no attribution rather than a guess.
func At(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, 512)
	n, _ := f.Read(buf)
	return format(string(buf[:n]))
}

// format turns a recorded "<pid> <argv>" into At's clause.
func format(content string) string {
	line := strings.TrimSpace(strings.SplitN(content, "\n", 2)[0])
	// The trim above leaves neither an empty pid nor a blank argv once the
	// line splits at a space.
	pid, argv, ok := strings.Cut(line, " ")
	if !ok {
		return ""
	}
	for _, r := range pid {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return fmt.Sprintf(" (pid %s: %s)", pid, strings.TrimSpace(argv))
}
