package session

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

var (
	csiRe   = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
	oscRe   = regexp.MustCompile(`\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)
	escRe   = regexp.MustCompile(`\x1b[()*+][0-9A-Za-z]|\x1b[=>78cDEHMNOZ\\^_]`)
	ctrlRe  = regexp.MustCompile(`[\x00-\x07\x0b\x0c\x0e-\x1a\x1c-\x1d\x1f\x7f]`)
	invalid = "\uFFFD"
)

// StripANSI removes terminal escape sequences from s.
func StripANSI(s string) string {
	if !strings.Contains(s, "\x1b") {
		return s
	}
	s = oscRe.ReplaceAllString(s, "")
	s = csiRe.ReplaceAllString(s, "")
	s = escRe.ReplaceAllString(s, "")
	return strings.ReplaceAll(s, "\x1b", "")
}

// Clean turns raw terminal bytes into readable text. It always normalises
// line endings and invalid UTF-8. When strip is true it also removes escape
// sequences and other control characters and applies a minimal terminal
// emulation for carriage returns and backspaces.
func Clean(raw []byte, strip bool) string {
	s := string(raw)
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, invalid)
	}
	if strip {
		s = StripANSI(s)
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if strip {
		s = applyLineEdits(s)
		s = ctrlRe.ReplaceAllString(s, "")
	} else {
		s = strings.ReplaceAll(s, "\r", "")
	}
	return s
}

// applyLineEdits emulates a dumb terminal: "\r" returns to the start of the
// line (later text overwrites earlier text) and "\b" erases one character.
func applyLineEdits(s string) string {
	if !strings.ContainsAny(s, "\r\b") {
		return s
	}
	var out strings.Builder
	var line []rune
	pos := 0
	flush := func() {
		out.WriteString(string(line))
		line = line[:0]
		pos = 0
	}
	for _, r := range s {
		switch r {
		case '\n':
			flush()
			out.WriteByte('\n')
		case '\r':
			pos = 0
		case '\b':
			if pos > 0 {
				pos--
			}
		default:
			if pos < len(line) {
				line[pos] = r
			} else {
				line = append(line, r)
			}
			pos++
		}
	}
	// Trailing text after a final bare "\r" that was never overwritten stays.
	flush()
	return out.String()
}

// Truncate keeps the head (25%) and tail (75%) of s when it exceeds max
// bytes, inserting a marker that states how many bytes were omitted.
func Truncate(s string, max int) (string, bool) {
	if max <= 0 || len(s) <= max {
		return s, false
	}
	head := max / 4
	tail := max - head
	for head > 0 && !utf8.RuneStart(s[head]) {
		head--
	}
	ts := len(s) - tail
	for ts < len(s) && !utf8.RuneStart(s[ts]) {
		ts++
	}
	omitted := ts - head
	return s[:head] + "\n[... " + itoa(omitted) + " bytes omitted ...]\n" + s[ts:], true
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
