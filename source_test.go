package main

import (
	"errors"
	"strings"
	"testing"
)

func apply(text string, edits []edit) string {
	for i := len(edits) - 1; i >= 0; i-- {
		e := edits[i]
		text = text[:e.Start] + e.Text + text[e.End:]
	}
	return text
}

func TestSortImports(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
		java              bool
		count             int
	}{
		{"Java groups and Unicode body",
			"\uFEFFpackage demo;\n\nimport zoo.B;\nimport a.A;\nimport a.A;\nimport static zoo.B.x;\nimport static a.A.*;\n\nclass Main { String s = \"é🙂\"; }\n",
			"\uFEFFpackage demo;\n\nimport a.A;\nimport a.A;\nimport zoo.B;\nimport static a.A.*;\nimport static zoo.B.x;\n\nclass Main { String s = \"é🙂\"; }\n", true, 2},
		{"Kotlin aliases, shebang and final newline",
			"#!/usr/bin/env kotlin\n\nimport z . B as Bee;\nimport a.A\nimport z.B as Other",
			"#!/usr/bin/env kotlin\n\nimport a.A\nimport z . B as Bee;\nimport z.B as Other", false, 1},
		{"Kotlin nested block comments",
			"/* outer\n/* inner */\nimport fake.Name\n*/\nimport z.Fixed\nimport z.B\nimport a.A\n",
			"/* outer\n/* inner */\nimport fake.Name\n*/\nimport z.Fixed\nimport a.A\nimport z.B\n", false, 1},
		{"Separate blank and comment groups",
			"import z.B;\nimport a.A;\n\n// attached\nimport z.Fixed;\nimport z.C;\nimport a.C;\n",
			"import a.A;\nimport z.B;\n\n// attached\nimport z.Fixed;\nimport a.C;\nimport z.C;\n", true, 2},
		{"Body raw string", "val x = \"\"\"\nimport z.B\nimport a.A\n\"\"\"\n", "", false, 0},
		{"Java raw escape", "// \\u000a\nimport z.B;\nimport a.A;\n", "", true, 0},
		{"Legacy CR", "import z.B;\r\nimport a.A;\n", "", true, 0},
		{"Kotlin escaped identifier", "import z.B\nimport a.A\nimport a.`while`\n", "", false, 0},
		{"Kotlin multiline import", "import z.B\nimport a.A\n    .Nested\n", "", false, 0},
		{"Kotlin file annotation", "@file:Suppress(\"test\")\nimport z.B\nimport a.A\n", "", false, 0},
		{"Inline import comment", "import z.B; // comment\nimport a.A;\n", "", true, 0},
		{"No-op", "import a.A;\nimport z.Z;\n", "", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			edits, err := sortImports(tc.input, tc.java, 1000)
			if err != nil || len(edits) != tc.count {
				t.Fatalf("got %v, %v", edits, err)
			}
			want := tc.want
			if tc.count == 0 {
				want = tc.input
			}
			if result := apply(tc.input, edits); result != want {
				t.Fatalf("got %q, want %q", result, want)
			}
			if edits, err := sortImports(want, tc.java, 1000); err != nil || len(edits) != 0 {
				t.Fatalf("sorting twice changed text: %v, %v", edits, err)
			}
		})
	}
	if edits, err := sortImports("import z.B\nimport a.B\n\nimport z.Z\nimport a.A", false, 1); !errors.Is(err, errTooManyEdits) || len(edits) != 0 {
		t.Fatalf("expected all-or-nothing limit, got %v, %v", edits, err)
	}
}

func TestSourceAndTemplates(t *testing.T) {
	for _, tc := range []struct{ uri, ext, contains string }{
		{"file:///workspace/%4Dain.java", ".java", "public class Main"},
		{"file:///workspace/String.java", ".java", "main(java.lang.String[] args)"},
		{"file:///workspace/Test.kt", ".kt", "fun main()"},
		{"file:///workspace/test.kts", ".kts", "\nmain()\n"},
		{"file:///workspace/class.java", ".java", ""},
		{"file:///workspace/a-b.java", ".java", ""},
		{"file:///workspace/Quoted%23.kt", ".kt", "fun main()"},
		{"file:///workspace/dir.kt/", "", ""},
		{"untitled:Main.java", "", ""},
	} {
		ext, name := sourceFile(tc.uri)
		if ext != tc.ext {
			t.Fatalf("%q: got %q, want %q", tc.uri, ext, tc.ext)
		}
		if ext == "" {
			continue
		}
		template, err := mainTemplate(ext, name)
		if tc.contains == "" && err == nil || tc.contains != "" && (err != nil || !strings.Contains(template, tc.contains)) {
			t.Fatalf("%q: got %q, %v", tc.uri, template, err)
		}
	}
}
