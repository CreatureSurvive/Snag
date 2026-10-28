package server

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

// Redaction levels for a project. The app already redacts before upload; the server runs the
// same rules again so old clients, custom clients and webhooks can't leak secrets into storage.
const (
	RedactStandard = ""       // tokens, keys, card numbers, URL credentials
	RedactStrict   = "strict" // standard plus emails, phone numbers and IP addresses
	RedactOff      = "off"
)

type redactRule struct {
	name  string
	re    *regexp.Regexp
	group int
	// valid rejects matches that only look sensitive (e.g. a digit run failing Luhn).
	valid func(text string, start, end int) bool
}

// The same rules as the Swift TextRedactor. RE2 has no lookaround, so the phone and IPv6 rules
// check their neighbors in code.
var (
	standardRules = []redactRule{
		{name: "private-key", re: regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`)},
		{name: "jwt", re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\b`)},
		{name: "bearer", re: regexp.MustCompile(`(?i)(?:bearer|token)\s+([A-Za-z0-9\-._~+/]{12,}=*)`), group: 1},
		{name: "secret", re: regexp.MustCompile(`(?i)(?:pass(?:word|wd)?|secret|api[_-]?key|access[_-]?key|auth[_-]?token|access[_-]?token|refresh[_-]?token|client[_-]?secret|session[_-]?id|x-emby-token|x-mediabrowser-token)["']?\s*[:=]\s*["']?([^\s"'&,;}]{3,})`), group: 1},
		{name: "credentials", re: regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://([^\s/:@]+:[^\s/@]+)@`), group: 1},
		{name: "card", re: regexp.MustCompile(`\b\d(?:[ -]?\d){12,18}\b`), valid: func(t string, s, e int) bool { return luhn(t[s:e]) }},
	}
	strictRules = append(append([]redactRule{}, standardRules...),
		redactRule{name: "email", re: regexp.MustCompile(`(?i)[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}`)},
		redactRule{name: "phone", re: regexp.MustCompile(`\+?\d{1,3}[ .-]?\(?\d{2,4}\)?[ .-]?\d{3,4}[ .-]?\d{3,4}`), valid: func(t string, s, e int) bool {
			return !wordOrDot(before(t, s)) && !wordOrDot(after(t, e))
		}},
		redactRule{name: "ip", re: regexp.MustCompile(`\b(?:(?:25[0-5]|2[0-4]\d|1?\d?\d)\.){3}(?:25[0-5]|2[0-4]\d|1?\d?\d)\b`)},
		redactRule{name: "ip", re: regexp.MustCompile(`(?:[0-9a-fA-F]{1,4}:){7}[0-9a-fA-F]{1,4}`), valid: func(t string, s, e int) bool {
			b, a := before(t, s), after(t, e)
			return b != ':' && !isWord(b) && a != ':' && !isWord(a)
		}},
	)
)

// Redactor applies a project's redaction level and counts what it removed.
type Redactor struct {
	rules   []redactRule
	Removed int
}

func NewRedactor(level string) *Redactor {
	switch level {
	case RedactOff:
		return &Redactor{}
	case RedactStrict:
		return &Redactor{rules: strictRules}
	}
	return &Redactor{rules: standardRules}
}

// Text scrubs one string. Already-redacted placeholders are left alone.
func (r *Redactor) Text(text string) string {
	for _, rule := range r.rules {
		text = r.apply(rule, text)
	}
	return text
}

func (r *Redactor) apply(rule redactRule, text string) string {
	matches := rule.re.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return text
	}
	var b strings.Builder
	last := 0
	for _, m := range matches {
		g := rule.group
		if 2*g+1 >= len(m) || m[2*g] < 0 {
			g = 0
		}
		start, end := m[2*g], m[2*g+1]
		if start < last || end <= start {
			continue
		}
		if rule.valid != nil && !rule.valid(text, start, end) {
			continue
		}
		if strings.HasPrefix(text[start:], "[redacted:") {
			continue
		}
		b.WriteString(text[last:start])
		b.WriteString("[redacted:" + rule.name + "]")
		last = end
		r.Removed++
	}
	if last == 0 {
		return text
	}
	b.WriteString(text[last:])
	return b.String()
}

// Record scrubs a report's text fields and its text attachments in dir (the staging directory),
// updating each file's size. The contact field is what the reporter chose to share and is kept.
func (r *Redactor) Record(rec *Record, dir string) error {
	if len(r.rules) == 0 {
		return nil
	}
	rec.Title = r.Text(rec.Title)
	rec.Message = r.Text(rec.Message)
	for k, v := range rec.Context.Custom {
		rec.Context.Custom[k] = r.Text(v)
	}
	for i, f := range rec.Files {
		if !f.IsText() {
			continue
		}
		path := filepath.Join(dir, f.Name)
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		before := r.Removed
		out := r.Text(string(data))
		if r.Removed == before {
			continue
		}
		if err := os.WriteFile(path, []byte(out), 0o640); err != nil {
			return err
		}
		rec.Files[i].Size = int64(len(out))
	}
	return nil
}

func luhn(candidate string) bool {
	var digits []int
	seen := map[int]bool{}
	for _, c := range candidate {
		if c >= '0' && c <= '9' {
			digits = append(digits, int(c-'0'))
			seen[int(c-'0')] = true
		}
	}
	if len(digits) < 13 || len(digits) > 19 || len(seen) < 2 {
		return false
	}
	sum := 0
	for i := range digits {
		d := digits[len(digits)-1-i]
		if i%2 == 1 {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
	}
	return sum%10 == 0
}

func before(t string, i int) rune {
	if i == 0 {
		return 0
	}
	return rune(t[i-1])
}

func after(t string, i int) rune {
	if i >= len(t) {
		return 0
	}
	return rune(t[i])
}

func isWord(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }

func wordOrDot(r rune) bool { return r == '.' || isWord(r) }
