package archguard

import (
	"slices"
	"strings"
	"testing"

	"github.com/nwverse/persona/server/internal/archguard/domainrules"
)

// plantedDirs is the planted tree's files in the given package directories (and not their subpackages).
func plantedDirs(t *testing.T, dirs ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for rel, text := range plantedFiles(t) {
		dir := rel[:strings.LastIndex(rel, "/")]
		if slices.Contains(dirs, dir) {
			out[rel] = text
		}
	}
	return out
}

// hook is a package's rules_test.go.
func hook(name string) string {
	return "package " + name + "_test\n\nimport (\n\t\"testing\"\n\n\t\"github.com/nwverse/persona/server/internal/archguard/domainrules\"\n)\n\n" +
		"func TestDomainRules(t *testing.T) { domainrules.Check(t) }\n"
}

// pkgFiles is a package at dir declaring layer, with a package comment, the hook, and body as its one file.
func pkgFiles(dir, layer, body string) map[string]string {
	name := dir[strings.LastIndex(dir, "/")+1:]
	return map[string]string{
		dir + "/doc.go":          "// Package " + name + " is a case.\n//\n//persona:layer " + layer + "\npackage " + name + "\n",
		dir + "/rules_test.go":   hook(name),
		dir + "/" + name + ".go": "package " + name + "\n\n" + body,
	}
}

// app is an application package at dir whose one file has body after an effect use.
func app(dir, imports, body string) map[string]string {
	files := pkgFiles(dir, "application", "import (\n\t\"sync\"\n"+imports+")\n\nfunc locked(mu *sync.Mutex) { mu.Lock() }\n\n"+body)
	delete(files, dir+"/rules_test.go")
	return files
}

// merged is the files of every map, later ones winning.
func merged(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// rules is the rule of each finding and count a case gives, sorted.
func rules(got []string) []string {
	out := []string{}
	for _, g := range got {
		out = append(out, strings.Fields(g)[0])
	}
	slices.Sort(out)
	return out
}

const fleetImport = "\t\"github.com/nwverse/persona/server/internal/assistant/fleetcount\"\n"

// The in-memory cases: what each one adds to a tree of compliant packages fails (or passes) exactly as written.
func TestDomainRuleCases(t *testing.T) {
	fleet := plantedDirs(t, "internal/agentref", "internal/assistant/fleetcount")
	core := func(body string) map[string]string { return pkgFiles("internal/assistant/sample", "core", body) }
	for _, c := range []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{"an undeclared new package", map[string]string{"internal/fresh/fresh.go": "package fresh\n"},
			[]string{"layer-declared"}},
		{"an application package without effects", pkgFiles("internal/calc", "application",
			"// Sum is a plus b.\nfunc Sum(a, b int) int { return a + b }\n"), []string{"layer-application"}},
		{"an application package with only a context.Context", pkgFiles("internal/calc", "application",
			"import \"context\"\n\n// Run stops when ctx does.\nfunc Run(ctx context.Context) error { return ctx.Err() }\n"),
			[]string{"layer-application"}},
		{"kit outside domainkit", pkgFiles("internal/words", "kit", ""), []string{"layer-kit"}},
		// The kit is every core's import, so its own code is held to a core's purity: no import beyond the computing
		// standard library and the kit (instant's time aside), no effect and no clock call.
		{"impure code in the kit", pkgFiles("internal/domainkit/extra", "kit", "import (\n\t\"os\"\n\t\"time\"\n\n"+
			"\t\"github.com/nwverse/persona/server/internal/proto\"\n)\n\nfunc read() bool {\n\tgo func() {}()\n"+
			"\treturn os.Getenv(\"X\") == \"\" && time.Now().IsZero() && proto.Version == \"\"\n}\n"),
			[]string{"core-clock-call", "core-effect", "core-import", "core-import", "core-import"}},
		// Every other core rule runs over the kit too: only kitAPI's lines, each one needed by the kit's listed API,
		// leave a rule alone.
		{"a setter, a constant, a tag, a map range and a stray function in the kit", pkgFiles("internal/domainkit/extra",
			"kit", "// Slot is a place. The zero Slot is the first.\ntype Slot struct{ slot int32 }\n\n"+
				"// Max is the last place.\nconst Max = 1<<31 - 1\n\n// Advance moves s on.\nfunc (s *Slot) Advance() { s.slot++ }\n\n"+
				"// Row is a stored place. The zero Row is the first.\ntype Row struct {\n\trow Slot `db:\"first\"`\n}\n\n"+
				"func count(m map[uint8]bool) int {\n\tn := 0\n\tfor range m {\n\t\tn++\n\t}\n\treturn n\n}\n\n"+
				"// First is the first place.\nfunc First() Slot { return Slot{} }\n"),
			[]string{"core-constructor", "core-exported-const", "core-map-range", "core-pointer-receiver", "core-tag"}},
		{"two layers in one package", merged(pkgFiles("internal/two", "library", ""), map[string]string{
			"internal/two/b.go": "// Package two again.\n//\n//persona:layer contract\npackage two\n"}),
			[]string{"layer-contract", "layer-one"}},
		{"core shared declaring a builder", pkgFiles("internal/shared", "core shared",
			"// Tag is a tag. The zero Tag is none.\ntype Tag struct{ tag uint8 }\n\n"+
				"// TagBuilder builds one. The zero TagBuilder is empty.\ntype TagBuilder struct{ tagBuilder Tag }\n"),
			[]string{"layer-shared"}},
		{"a missing hook", map[string]string{"internal/assistant/nohook/doc.go": "// Package nohook is a core.\n//\n" +
			"//persona:layer core\npackage nohook\n"}, []string{"core-hook"}},
		{"dot-import and alias", merged(fleet, app("internal/assistant/sample", ". "+fleetImport,
			"var working = StateWorking()\n"), map[string]string{"internal/assistant/sample/alias.go": "package sample\n\n" +
			"import \"github.com/nwverse/persona/server/internal/assistant/fleetcount\"\n\ntype State = fleetcount.State\n"}),
			[]string{"out-alias", "out-dot-import"}},
		{"a zero literal in a test", merged(fleet, app("internal/assistant/sample", "", ""), map[string]string{
			"internal/assistant/sample/sample_test.go": "package sample\n\nimport \"github.com/nwverse/persona/server/internal/" +
				"assistant/fleetcount\"\n\nvar zero = fleetcount.State{}\n"}), []string{"out-literal"}},
		{"a conversion fleetcount.Role(x)", merged(fleet, app("internal/assistant/sample", fleetImport,
			"func role(s fleetcount.State) fleetcount.Role { return fleetcount.Role(s) }\n")), []string{"out-conversion"}},
		{"a method value m.Wire", merged(fleet, app("internal/assistant/sample", fleetImport,
			"func count(n fleetcount.RowCount) func() int { return n.Wire }\n")), []string{"out-wire-ref"}},
		{"a method expression agentref.MachineID.Wire", merged(fleet, app("internal/assistant/sample",
			"\t\"github.com/nwverse/persona/server/internal/agentref\"\n", "var unwrap = agentref.MachineID.Wire\n")),
			[]string{"out-wire-ref"}},
		{"pkg.Wire{} where pkg is an import passes", merged(fleet, app("internal/assistant/sample",
			"\t\"github.com/nwverse/persona/server/internal/assistantwire\"\n", "var w = assistantwire.Wire{}\n")), []string{}},
		{"a visitor method in application code", merged(fleet, app("internal/assistant/sample",
			fleetImport+"\t\"github.com/nwverse/persona/server/internal/domainkit/instant\"\n"+
				"\t\"github.com/nwverse/persona/server/internal/domainkit/slot\"\n",
			"type view struct{ rows int }\n\nfunc (x *view) VisitAgent(a slot.Slot, w fleetcount.Why, s instant.Instant) {}\n")),
			[]string{"out-visitor-impl"}},
		{"a type parameter in a core", core("func same[H comparable](a, b H) bool { return a == b }\n"),
			[]string{"core-type-param"}},
		{"a package var in a core", core("var seen int\n"), []string{"core-package-var"}},
		// Every clock selector the timeline's own purity test banned before core-clock-call replaced it, called or not.
		{"clock calls in a core", core("import \"time\"\n\nfunc clocks() {\n\tnow, since, until := time.Now(), time.Since, time.Until\n" +
			"\tafter, afterFunc, tick := time.After, time.AfterFunc, time.Tick\n\ttimer, ticker := time.NewTimer, time.NewTicker\n" +
			"\tzone, err := time.LoadLocation(\"UTC\")\n\ttime.Sleep(0)\n" +
			"\t_, _, _, _, _, _, _, _, _, _ = now, since, until, after, afterFunc, tick, timer, ticker, zone, err\n}\n"),
			[]string{"core-clock-call", "core-clock-call", "core-clock-call", "core-clock-call", "core-clock-call",
				"core-clock-call", "core-clock-call", "core-clock-call", "core-clock-call", "core-clock-call", "core-import"}},
		{"a map range in a core", core("func count(m map[uint8]bool) int {\n\tn := 0\n\tfor range m {\n\t\tn++\n\t}\n\treturn n\n}\n"),
			[]string{"core-map-range"}},
		{"a receiver index write", core("// Tags is a list. The zero Tags is none.\ntype Tags struct{ tags []Tag }\n\n" +
			"// Tag is a tag. The zero Tag is none.\ntype Tag struct{ tag uint8 }\n\n" +
			"func (t Tags) first(g Tag) Tags {\n\tt.tags[0] = g\n\treturn t\n}\n"), []string{"core-receiver-write"}},
		{"a getter with a dummy parameter", core("// Note is a note. The zero Note is none.\ntype Note struct {\n\ttitle Tag\n\tbody  Tag\n}\n\n" +
			"// Tag is a tag. The zero Tag is none.\ntype Tag struct{ tag uint8 }\n\n" +
			"// Title hands the title out.\nfunc (n Note) Title(_ Tag) Tag { return n.title }\n"), []string{"core-accessor"}},
		{"Decide with a sealed Command passes", plantedDirs(t, "internal/assistant/inbox/item"), []string{}},
		{"another package's interface as a parameter", merged(fleet, core("import \"github.com/nwverse/persona/server/internal/agentref\"\n\n"+
			"// Tag is a tag. The zero Tag is none.\ntype Tag struct{ tag uint8 }\n\n"+
			"// Tell tells v nothing.\nfunc (g Tag) Tell(v agentref.RefVisitor) {}\n")), []string{"core-interface-param"}},
		{"the hub importing a core", merged(fleet, app("internal/hub", fleetImport, "var badge = fleetcount.SurfaceBadge()\n")),
			[]string{"path-no-core"}},
		// An adapter's Wire hands application code a domain value's raw shape, called or passed on; tests are not counted.
		{"an adapter's Wire passed on in undeclared code", merged(fleet, plantedDirs(t, "internal/agentref/agentrefadapt"),
			map[string]string{"internal/legacy/legacy.go": "package legacy\n\nimport \"github.com/nwverse/persona/server/internal/" +
				"agentref/agentrefadapt\"\n\nvar unwrap = agentrefadapt.Wire\n"}), []string{"app-bridge-call", "layer-declared"}},
		{"an adapter's Wire in a test passes", merged(fleet, plantedDirs(t, "internal/agentref/agentrefadapt"),
			app("internal/assistant/sample", "", ""), map[string]string{"internal/assistant/sample/sample_test.go": "package sample\n\n" +
				"import (\n\t\"github.com/nwverse/persona/server/internal/agentref\"\n\t\"github.com/nwverse/persona/server/internal/" +
				"agentref/agentrefadapt\"\n)\n\nfunc wire(r agentref.Ref) { _ = agentrefadapt.Wire(r) }\n"}), []string{}},
		// A call written as a statement drops its results when the parsed declarations say it has some: another
		// core's method, an interface method; one they cannot tell (the standard library's, a name declared both
		// ways) is left alone.
		{"calls written as statements in a core", merged(fleet, core("import \"github.com/nwverse/persona/server/internal/agentref\"\n\n"+
			"// Tag is a tag. The zero Tag is none.\ntype Tag struct{ tag uint8 }\n\n"+
			"// next is what follows a tag.\ntype next interface{ following() Tag }\n\n"+
			"func skip(n next, r agentref.Ref) {\n\tr.IsSame(r)\n\tn.following()\n}\n")),
			[]string{"core-outcome-return", "core-outcome-return"}},
		{"calls the declarations cannot resolve pass", core("import \"strings\"\n\n" +
			"// Tag is a tag. The zero Tag is none.\ntype Tag struct{ tag uint8 }\n\nfunc (t Tag) mark() {}\n\n" +
			"// marker is something marked.\ntype marker interface{ mark() Tag }\n\n" +
			"func keep(m marker, raw string) {\n\tstrings.ToUpper(raw)\n\tm.mark()\n}\n"), []string{}},
		{"a library importing the kit", pkgFiles("internal/lib", "library",
			"import \"github.com/nwverse/persona/server/internal/domainkit/instant\"\n\nvar zero instant.Instant\n"),
			[]string{"layer-library"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, findings, _ := domainCase(t, c.files, []string{"internal/assistantwire"})
			if have := rules(got); !slices.Equal(have, c.want) {
				var msgs []string
				for _, f := range findings {
					msgs = append(msgs, f.String())
				}
				t.Errorf("got %q, want %q:\n%s", have, c.want, strings.Join(msgs, "\n"))
			}
		})
	}
}

// legacyCore is a core that predates the rules: two package variables, a frozen import of time, and an application
// package that builds one of its types by literal.
func legacyCore() map[string]string {
	return merged(pkgFiles("internal/legacy", "core", "import \"time\"\n\nvar a, b int\n\nfunc since(t time.Time) bool { return t.IsZero() }\n\n"+
		"// Row is a row. The zero Row is none.\ntype Row struct{ row uint8 }\n"),
		app("internal/user", "\t\"github.com/nwverse/persona/server/internal/legacy\"\n", "var r = legacy.Row{}\n"))
}

// judge runs every rule over files and holds them to the lists' text.
func judge(t *testing.T, files map[string]string, undeclared, edges, baseline string, base *domainrules.Measure) domainrules.Verdict {
	t.Helper()
	_, findings, counts := domainCase(t, files, nil)
	lists := domainrules.ParseLists("", undeclared, edges, baseline)
	all := merged(plantedKit(t), files)
	pkgs, err := domainrules.LoadFiles(serverModule, all)
	if err != nil {
		t.Fatal(err)
	}
	return domainrules.Judge(findings, counts, pkgs, lists, base, "")
}

// The lists hold the legacy code: counts that only fall (with a base, never above the merge base's either), frozen
// imports and frozen undeclared packages, each reporting what can be tidied.
func TestDomainLists(t *testing.T) {
	files := legacyCore()
	const edges = "internal/legacy imports time\n"
	const baseline = "internal/legacy core-package-var 2\ninternal/legacy out-literal 1\n"
	if v := judge(t, files, "", edges, baseline, nil); len(v.Failures)+len(v.Notices) != 0 {
		t.Errorf("the legacy core at its baseline: %q, %q", v.Failures, v.Notices)
	}

	// A rise fails and names the findings; a fall is a notice saying how to lower.
	v := judge(t, files, "", edges, "internal/legacy core-package-var 1\ninternal/legacy out-literal 1\n", nil)
	if len(v.Failures) != 3 || !strings.Contains(v.Failures[2], "2 now, above its limit (baseline 1)") {
		t.Errorf("a baseline rise: %q", v.Failures)
	}
	v = judge(t, files, "", edges, "internal/legacy core-package-var 3\ninternal/legacy out-literal 1\n", nil)
	if len(v.Failures) != 0 || len(v.Notices) != 1 || !strings.Contains(v.Notices[0], "lower_baselines.py --only domain-rules") {
		t.Errorf("a count under its baseline: %q, %q", v.Failures, v.Notices)
	}

	// The importer's literal counts against the legacy core it builds.
	v = judge(t, files, "", edges, "internal/legacy core-package-var 2\n", nil)
	if len(v.Failures) != 2 || !strings.Contains(v.Failures[0], "RULE out-literal") ||
		!strings.Contains(v.Failures[1], "internal/legacy out-literal: 1 now") {
		t.Errorf("an importer literal of a legacy core: %q", v.Failures)
	}

	// With a base, a count may not rise above the merge base's.
	base := &domainrules.Measure{Counts: map[string]int{"internal/legacy core-package-var": 1, "internal/legacy out-literal": 1}}
	v = judge(t, files, "", edges, baseline, base)
	if len(v.Failures) != 3 || !strings.Contains(v.Failures[2], "merge base 1, baseline 2") {
		t.Errorf("a rise above the merge base: %q", v.Failures)
	}

	// A swapped import fails, and the frozen one that left is a notice.
	swapped := merged(files, map[string]string{"internal/legacy/legacy.go": strings.Replace(files["internal/legacy/legacy.go"],
		"import \"time\"\n\nvar a, b int\n\nfunc since(t time.Time) bool { return t.IsZero() }\n",
		"import \"os\"\n\nvar a, b int\n\nfunc since(name string) bool { return os.Getenv(name) == \"\" }\n", 1)})
	v = judge(t, swapped, "", edges, baseline, nil)
	if len(v.Failures) != 1 || !strings.Contains(v.Failures[0], "RULE core-import internal/legacy/legacy.go imports os") ||
		len(v.Notices) != 1 || !strings.Contains(v.Notices[0], "imports time no longer happens") {
		t.Errorf("a swapped legacy import: %q, %q", v.Failures, v.Notices)
	}

	// A rule without a count takes no baseline line.
	v = judge(t, files, "", edges, "internal/legacy core-hook 1\n"+baseline, nil)
	if len(v.Failures) != 1 || !strings.Contains(v.Failures[0], "core-hook takes no count") {
		t.Errorf("a strict rule in the baseline: %q", v.Failures)
	}

	// Undeclared packages: a frozen one passes, a new one fails, one that declares now or is gone is a notice.
	fresh := map[string]string{"internal/old/old.go": "package old\n", "internal/fresh/fresh.go": "package fresh\n"}
	v = judge(t, merged(files, fresh), "internal/gone\ninternal/legacy\ninternal/old\ninternal/user\ninternal/waiting #77\n", edges, baseline, nil)
	if len(v.Failures) != 1 || !strings.Contains(v.Failures[0], "RULE layer-declared internal/fresh declares no layer") ||
		len(v.Notices) != 4 {
		t.Errorf("frozen undeclared packages: %q, %q", v.Failures, v.Notices)
	}
}

// An allowance is one open PR's room: it names the type the PR adds and counts only while that type is in the
// package, so no other change can spend it. Once the type is on main the count takes it in; with a base, the merge
// base's count holds what it has already.
func TestDomainAllowances(t *testing.T) {
	files := merged(legacyCore(), app("internal/seats", "", "// Seat is a record.\ntype Seat struct{ Name string }\n"))
	const legacy = "internal/legacy core-package-var 2\ninternal/legacy out-literal 1\n"
	const edges = "internal/legacy imports time\n"
	const id = "internal/seats app-exported-record"
	v := judge(t, files, "", edges, legacy, nil)
	if len(v.Failures) != 1 || !strings.Contains(v.Failures[0], "RULE app-exported-record internal/seats adds an exported-field record (count 1 > 0)") {
		t.Errorf("a new record with no line: %q", v.Failures)
	}
	v = judge(t, files, "", edges, legacy+id+" Seat #4512\n", nil)
	if len(v.Failures) != 0 || len(v.Notices) != 1 || !strings.Contains(v.Notices[0], "Seat (#4512) is in internal/seats now: take it into the count with python3 scripts/ci/lower_baselines.py --only domain-rules") {
		t.Errorf("the allowed type: %q, %q", v.Failures, v.Notices)
	}
	// The room is the named type's: another change's record does not get it.
	v = judge(t, files, "", edges, legacy+id+" Desk #4512\n", nil)
	if len(v.Failures) != 1 || !strings.Contains(v.Failures[0], "(count 1 > 0)") || len(v.Notices) != 1 ||
		!strings.Contains(v.Notices[0], "internal/seats app-exported-record allows Desk (#4512), not here yet; once a type's PRs have all closed, python3 scripts/test/name_freezes.py pending --write --only domain_layers_baseline drops its line") {
		t.Errorf("another type than the allowed one: %q, %q", v.Failures, v.Notices)
	}
	// With a base: the merge base without the type leaves the room; one that has it (the PR merged) holds the count.
	v = judge(t, files, "", edges, legacy+id+" Seat #4512\n", &domainrules.Measure{Counts: map[string]int{id: 0}})
	if len(v.Failures) != 0 {
		t.Errorf("the allowed type, the merge base without it: %q", v.Failures)
	}
	more := merged(files, map[string]string{"internal/seats/desk.go": "package seats\n\n// Desk is another record.\ntype Desk struct{ Name string }\n"})
	landed := &domainrules.Measure{Counts: map[string]int{id: 1}, Names: map[string]map[string]bool{id: {"Seat": true}}}
	v = judge(t, more, "", edges, legacy+id+" 1\n"+id+" Seat #4512\n", landed)
	if len(v.Failures) != 1 || !strings.Contains(v.Failures[0], "(count 2 > 1)") {
		t.Errorf("a record beside an allowed type the merge base has: %q", v.Failures)
	}
	// Only the counts of named types take an allowance.
	lists := domainrules.ParseLists("", "", "", "internal/legacy core-package-var Seat #4512\n")
	if len(lists.Problems) != 1 || !strings.Contains(lists.Problems[0], "is an allowance for core-package-var, which takes none") {
		t.Errorf("an allowance for a finding: %q", lists.Problems)
	}
}

// The lists' format, and lowering: counts fall to the tree's, an allowance whose type landed is taken into its count,
// one still waiting stays, and a count gone to zero leaves; no limit rises.
func TestDomainListFormat(t *testing.T) {
	for _, c := range []struct{ baseline, problem string }{
		{"b core-any 1\na core-any 1\n", "line 2 (a core-any) is out of order"},
		{"a core-any 1\na core-any 2\n", "line 2 repeats a core-any (line 1)"},
		{"a app-exported-record B #1\na app-exported-record A #2\n", "line 2 (a app-exported-record) is out of order"},
		{"a app-exported-record A #1\na app-exported-record 2\n", "line 2 (a app-exported-record) is out of order"},
		{"a app-exported-record A #1\na app-exported-record A #2\n", "line 2 repeats a app-exported-record A (line 1)"},
		{"a no-such-rule 1\n", "line 1 names no rule: no-such-rule"},
		{"a core-any one\n", "line 1 is neither <key> <rule> <count> nor an allowance"},
		{"a core-any 4 #12\n", "line 1 is neither <key> <rule> <count> nor an allowance"},
		{"a app-exported-record Seat\n", "line 1 is neither <key> <rule> <count> nor an allowance"},
		{"a app-exported-record Seat 12\n", "line 1 is neither <key> <rule> <count> nor an allowance"},
	} {
		lists := domainrules.ParseLists("", "", "", c.baseline)
		if !slices.ContainsFunc(lists.Problems, func(p string) bool { return strings.Contains(p, c.problem) }) {
			t.Errorf("%q: problems %q, want %q", c.baseline, lists.Problems, c.problem)
		}
	}
	lists := domainrules.ParseLists("", "b\na\n", "", "")
	if len(lists.Problems) != 1 || !strings.Contains(lists.Problems[0], "line 2 (a) is out of order") {
		t.Errorf("an unsorted freeze list: %q", lists.Problems)
	}
	const text = "# header\n\na core-any 3\na core-package-var 2\nb app-exported-record 4\nb app-exported-record Landed #12\n" +
		"b app-exported-record Waiting #13 #15\nc core-tag 1\nd app-named-primitive Fresh #14\ne app-named-primitive 2\n" +
		"e app-named-primitive Early #16\n"
	m := domainrules.Measure{
		Counts: map[string]int{"a core-any": 2, "a core-package-var": 5, "b app-exported-record": 5,
			"d app-named-primitive": 1, "e app-named-primitive": 2},
		Names: map[string]map[string]bool{"b app-exported-record": {"Landed": true, "Other": true},
			"d app-named-primitive": {"Fresh": true}, "e app-named-primitive": {"Early": true, "Kept": true}},
	}
	lists = domainrules.ParseLists("", "", "", text)
	if len(lists.Problems) != 0 {
		t.Fatalf("problems: %q", lists.Problems)
	}
	want := "# header\na core-any 2\na core-package-var 2\nb app-exported-record 5\nb app-exported-record Waiting #13 #15\n" +
		"d app-named-primitive 1\ne app-named-primitive 2\n"
	if lowered := lists.Baseline.Lowered(text, m); lowered != want {
		t.Errorf("Lowered = %q, want %q", lowered, want)
	}
}

// The hook checks one package: its own findings and the ones its importers' code counts against it, never another
// package's.
func TestDomainHookScope(t *testing.T) {
	all := merged(plantedKit(t), plantedFiles(t))
	pkgs, err := domainrules.LoadFiles(serverModule, all)
	if err != nil {
		t.Fatal(err)
	}
	idx := domainrules.NewIndex(serverModule, pkgs, plantedContracts)
	for _, c := range []struct {
		scope string
		want  []string // the rules of its failures, sorted
	}{
		{"internal/assistant/hookless", []string{"core-hook"}},
		{"internal/assistant/inbox/item", []string{}},
		{"internal/assistant/quiethours", []string{}},
		{"internal/assistant/nofamily", []string{"core-fail-family"}},
		// fleetcount's own code passes; what its importers do to it is its hook's business too.
		{"internal/assistant/fleetcount", []string{"core-importers", "out-alias", "out-conversion", "out-dot-import",
			"out-literal", "out-unsafe", "out-unsafe", "out-visitor-impl", "path-no-core"}},
	} {
		findings, _, err := domainrules.Run(pkgs, idx, c.scope)
		if err != nil {
			t.Fatal(err)
		}
		v := domainrules.Judge(findings, nil, pkgs, domainrules.Lists{}, nil, c.scope)
		got := []string{}
		for _, f := range v.Failures {
			if _, rest, ok := strings.Cut(f, "RULE "); ok {
				got = append(got, strings.Fields(rest)[0])
			}
		}
		slices.Sort(got)
		if !slices.Equal(got, c.want) {
			t.Errorf("the hook in %s: %q, want %q", c.scope, v.Failures, c.want)
		}
	}
}
