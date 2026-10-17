package cmd

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestParseDotenvValue: the value compose takes from the text after KEY=.
func TestParseDotenvValue(t *testing.T) {
	for raw, want := range map[string]string{
		`full,agentic`:                  "full,agentic",
		`"full,agentic,ingress"`:        "full,agentic,ingress",
		`'full,logs'`:                   "full,logs",
		`full,logs # the log stack`:     "full,logs",
		"full,logs\t# the log stack":    "full,logs",
		`"full,logs" # quoted`:          "full,logs",
		`'full' # quoted`:               "full",
		`"a # b"`:                       "a # b",
		`'a # b'`:                       "a # b",
		`a#b`:                           "a#b",
		`#abc`:                          "#abc",
		` # nothing`:                    "",
		``:                              "",
		`   spaced   `:                  "spaced",
		`"unterminated`:                 `"unterminated`,
		`"a\"b"`:                        `a"b`,
		`"a\\b"`:                        `a\b`,
		`"line\nbreak"`:                 "line\nbreak",
		`'no \n escapes'`:               `no \n escapes`,
		`x "y"`:                         `x "y"`,
		`https://cp.example.com/a#frag`: "https://cp.example.com/a#frag",
	} {
		if got := parseDotenvValue(raw); got != want {
			t.Errorf("parseDotenvValue(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestParseDotenvLine(t *testing.T) {
	for line, want := range map[string][2]string{
		"KEY=value":              {"KEY", "value"},
		"  KEY=value":            {"KEY", "value"},
		"export KEY=value":       {"KEY", "value"},
		"export\tKEY='v'":        {"KEY", "v"},
		"KEY = value":            {"KEY", "value"},
		"KEY=value\r":            {"KEY", "value"},
		"KEY=":                   {"KEY", ""},
		"KEY=a=b":                {"KEY", "a=b"},
		"exportKEY=1":            {"exportKEY", "1"},
		"export=1":               {"export", "1"},
		"# KEY=value":            {},
		"":                       {},
		"no equals sign":         {},
		"=value":                 {},
		"two words=value":        {},
		"COMPOSE_PROFILES='a,b'": {"COMPOSE_PROFILES", "a,b"},
	} {
		k, v, ok := parseDotenvLine(line)
		if ok != (want != [2]string{}) || k != want[0] || v != want[1] {
			t.Errorf("parseDotenvLine(%q) = %q, %q, %v; want %v", line, k, v, ok, want)
		}
	}
}

// TestDotenvValue: the last KEY= line wins, as compose reads .env, and
// quotes and comments are not part of the value; composeProfiles and the
// project's name read it that way.
func TestDotenvValue(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	writeFile(t, env, "# COMPOSE_PROFILES=nope\nCOMPOSE_PROFILES=first\nOTHER=\"x\"\nexport COMPOSE_PROFILES=\"full, logs ,loki\" # the plane\nCOMPOSE_PROJECT_NAME='My_Plane' # name\n")
	if got := dotenvValue(env, "COMPOSE_PROFILES"); got != "full, logs ,loki" {
		t.Fatalf("COMPOSE_PROFILES = %q", got)
	}
	if got := composeProfiles(env); !slices.Equal(got, []string{"full", "logs", "loki"}) {
		t.Fatalf("composeProfiles = %v", got)
	}
	if got := composeProjectName(dir, env); got != "my_plane" {
		t.Fatalf("composeProjectName = %q", got)
	}
	if got := dotenvValue(env, "MISSING"); got != "" {
		t.Fatalf("MISSING = %q", got)
	}
	if got := dotenvValue(filepath.Join(dir, "none"), "COMPOSE_PROFILES"); got != "" {
		t.Fatalf("no file: %q", got)
	}
	// The raw text is what the CLI carries forward for its own values.
	if got := readEnvVar(env, "OTHER"); got != `"x"` {
		t.Fatalf("readEnvVar = %q", got)
	}
}

// TestDeployProfiles: an in-place deploy turns full, agentic and ingress on
// and off by its answers, and keeps every other profile the plane runs.
func TestDeployProfiles(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		before                 []string
		full, agentic, ingress bool
		want                   []string
	}{
		{"a new plane", nil, true, false, false, []string{"full"}},
		{"everything", nil, true, true, true, []string{"full", "agentic", "ingress"}},
		{"the log stack stays", []string{"full", "logs", "loki"}, true, false, false, []string{"full", "logs", "loki"}},
		{"journeys off, the log stack stays", []string{"full", "logs", "loki"}, false, false, false, []string{"logs", "loki"}},
		{"the log stack first", []string{"logs", "full", "agentic"}, true, true, false, []string{"full", "agentic", "logs"}},
		{"an unknown profile stays", []string{"debug", "full"}, true, false, true, []string{"full", "ingress", "debug"}},
		{"a repeated profile is kept once", []string{"logs", "logs", "full"}, true, false, false, []string{"full", "logs"}},
		{"nothing", []string{"full", "agentic", "ingress"}, false, false, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := deployProfiles(tc.before, tc.full, tc.agentic, tc.ingress); !slices.Equal(got, tc.want) {
				t.Fatalf("deployProfiles = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestInPlaceImage: what a file written in place first holds, before it is
// cut to size: the new text then blank lines up to the old length, so a
// stop in between leaves the new .env, not the new text followed by the end
// of the old one (compose reads the last KEY= line).
func TestInPlaceImage(t *testing.T) {
	old := "A=1\nDP_IMAGE_TAG=0.1.19\nORIGAMY_RESET_PENDING=0.1.20\nCOMPOSE_PROFILES=full\n"
	for _, tc := range []struct{ name, data string }{
		{"shorter", "A=1\nDP_IMAGE_TAG=0.1.20\n"},
		{"as long", strings.Repeat("x", len(old)-1) + "\n"},
		{"longer", old + "B=2\n"},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			img := string(inPlaceImage([]byte(tc.data), int64(len(old))))
			if !strings.HasPrefix(img, tc.data) || strings.Trim(img[len(tc.data):], "\n") != "" || len(img) < len(tc.data) || (len(img) != max(len(old), len(tc.data))) {
				t.Fatalf("image %q for %q over %d bytes", img, tc.data, len(old))
			}
			// Read as compose reads it, it is the new file.
			dir := t.TempDir()
			p := filepath.Join(dir, ".env")
			writeFile(t, p, img)
			for _, k := range []string{"A", "DP_IMAGE_TAG", "ORIGAMY_RESET_PENDING", "COMPOSE_PROFILES", "B"} {
				w := filepath.Join(dir, "want")
				writeFile(t, w, tc.data)
				if dotenvValue(p, k) != dotenvValue(w, k) {
					t.Fatalf("%s = %q, want %q", k, dotenvValue(p, k), dotenvValue(w, k))
				}
			}
		})
	}
}
