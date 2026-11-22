package main

import (
	"errors"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

type edit struct {
	Start int    `json:"start"`
	End   int    `json:"end"`
	Text  string `json:"text"`
}

func sourceFile(uri string) (string, string) {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" || u.Host != "" || !strings.HasPrefix(u.Path, "/") ||
		strings.HasSuffix(u.Path, "/") || u.RawQuery != "" || u.Fragment != "" || strings.ContainsRune(u.Path, 0) {
		return "", ""
	}
	name := path.Base(u.Path)
	return path.Ext(name), name
}

var (
	simpleClass = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	javaWords   = strings.Fields("abstract assert boolean break byte case catch char class const continue default do double else enum extends final finally float for goto if implements import instanceof int interface long native new package private protected public return short static strictfp super switch synchronized this throw throws transient try void volatile while true false null _ var yield record sealed permits")
)

func mainTemplate(extension, name string) (string, error) {
	switch extension {
	case ".java":
		name = strings.TrimSuffix(name, path.Ext(name))
		if !simpleClass.MatchString(name) || slices.Contains(javaWords, name) {
			return "", errors.New("Use a .java filename with a simple, non-keyword Java class name.")
		}
		stringType := "String"
		if name == stringType {
			stringType = "java.lang.String"
		}
		return "public class " + name + " {\n    public static void main(" + stringType + "[] args) {\n    }\n}\n", nil
	case ".kt":
		return "fun main() {\n}\n", nil
	default:
		return "fun main() {\n}\n\nmain()\n", nil
	}
}

const identifier = `[A-Za-z_$][A-Za-z0-9_$]*`
const qualified = identifier + `(?:[ \t]*\.[ \t]*` + identifier + `)*`
const importPath = qualified + `(?:[ \t]*\.[ \t]*\*)?`

var (
	javaPackage        = regexp.MustCompile(`^package[ \t]+` + qualified + `[ \t]*;$`)
	javaImport         = regexp.MustCompile(`^import[ \t]+(static[ \t]+)?(` + importPath + `)[ \t]*;$`)
	kotlinPackage      = regexp.MustCompile(`^package[ \t]+` + qualified + `[ \t]*;?$`)
	headerContinuation = regexp.MustCompile(`^(?:package|import|as)(?:[^A-Za-z0-9_$]|$)|^[@]file|^[.;]`)
	importWhitespace   = strings.NewReplacer(" ", "", "\t", "")
	kotlinImport       = regexp.MustCompile(`^import[ \t]+(` + importPath + `)(?:[ \t]+as[ \t]+` + identifier + `)?[ \t]*;?$`)
)

type importLine struct {
	text, key string
	start     int
}

// Sort only complete, single-line imports in the source header. Comments and
// blank lines delimit groups. Keep the first import below a comment in place.
func sortImports(text string, java bool, maxEdits int) ([]edit, error) {
	if strings.ContainsRune(text, '\r') {
		return nil, nil
	}
	if java && strings.Contains(text, `\u`) {
		// Java can translate Unicode escapes into tokens before lexical analysis.
		return nil, nil
	}
	var edits []edit
	var group []importLine
	var previousStatic, attached, sawPackage, sawImport bool
	depth := 0
	flush := func() bool {
		if len(group) > 1 && !slices.IsSortedFunc(group, compareImports) {
			if len(edits) == maxEdits {
				return false
			}
			start := group[0].start
			last := group[len(group)-1]
			end := last.start + len(last.text)
			slices.SortStableFunc(group, compareImports)
			var replacement strings.Builder
			replacement.Grow(end - start)
			for i, line := range group {
				if i > 0 {
					replacement.WriteByte('\n')
				}
				replacement.WriteString(line.text)
			}
			edits = append(edits, edit{start, end, replacement.String()})
		}
		group = group[:0]
		return true
	}

	var offset int
	for remaining := text; remaining != ""; {
		raw, tail, newline := strings.Cut(remaining, "\n")
		start := offset
		offset += len(raw)
		if newline {
			offset++
		}
		remaining = tail
		firstLine := start == 0
		if firstLine && strings.HasPrefix(raw, "\uFEFF") {
			raw = strings.TrimPrefix(raw, "\uFEFF")
			start += len("\uFEFF")
		}
		line := strings.Trim(raw, " \t")
		switch {
		case depth > 0 || strings.HasPrefix(line, "/*"):
			if !flush() {
				return nil, errTooManyEdits
			}
			var complete bool
			depth, complete = commentLine(line, depth, !java)
			if !complete {
				return nil, nil
			}
			attached = true
		case strings.HasPrefix(line, "//") || (!java && firstLine && strings.HasPrefix(line, "#!")):
			if !flush() {
				return nil, errTooManyEdits
			}
			attached = true
		case line == "":
			if !flush() {
				return nil, errTooManyEdits
			}
			attached = false
		default:
			packagePattern, importPattern := kotlinPackage, kotlinImport
			if java {
				packagePattern, importPattern = javaPackage, javaImport
			}
			if !sawPackage && !sawImport && packagePattern.MatchString(line) {
				sawPackage = true
				attached = false
				continue
			}
			match := importPattern.FindStringSubmatch(line)
			if match == nil {
				// Leave headers with unsupported syntax alone. A continuation
				// may extend the previous line in Kotlin.
				if headerContinuation.MatchString(line) {
					return nil, nil
				}
				remaining = ""
				continue
			}
			sawImport = true
			isStatic := java && match[1] != ""
			if isStatic != previousStatic && !flush() {
				return nil, errTooManyEdits
			}
			previousStatic = isStatic
			if attached {
				attached = false
				continue
			}
			key := importWhitespace.Replace(match[len(match)-1])
			group = append(group, importLine{raw, key, start})
		}
	}
	if depth != 0 {
		return nil, nil
	}
	if !flush() {
		return nil, errTooManyEdits
	}
	return edits, nil
}

var errTooManyEdits = errors.New("Too many import groups for one atomic edit.")

func compareImports(a, b importLine) int { return strings.Compare(a.key, b.key) }

// Return false if source follows a block comment on the same line.
func commentLine(line string, depth int, nested bool) (int, bool) {
	for i := 0; i < len(line); {
		switch {
		case (depth == 0 || nested) && strings.HasPrefix(line[i:], "/*"):
			depth++
			i += 2
		case depth > 0 && strings.HasPrefix(line[i:], "*/"):
			depth--
			i += 2
			if depth == 0 {
				return 0, strings.Trim(line[i:], " \t") == ""
			}
		default:
			i++
		}
	}
	return depth, true
}

func boundary(text string, offset uint64) bool {
	return offset == uint64(len(text)) || offset < uint64(len(text)) && utf8.RuneStart(text[offset])
}
