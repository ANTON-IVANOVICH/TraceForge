// Package deploy_test checks the things about a deployment that only rot.
//
// A Kubernetes manifest, a Grafana dashboard and an alert rule share a property
// that makes them uniquely dangerous: they are not compiled, not linked, and not
// executed by any test. A flag renamed in Go keeps the manifest that passes the
// old one syntactically perfect. A metric renamed in Go leaves the dashboard
// panel showing "No data" — for a year, because nobody notices the absence of a
// line on a graph they only look at during an incident. An alert's runbook_url
// keeps pointing at a file somebody deleted, and the person who finds out is on
// call at three in the morning.
//
// So the deployment artefacts are treated here as what they are: source code with
// no compiler. These tests are the compiler.
//
// Note what is NOT here. Nothing validates the YAML against the Kubernetes API
// schema — `kubeconform` does that in CI, and re-implementing it would be worse.
// Nothing renders the Helm chart — `helm template` does that in CI, and it needs
// helm. What is here is everything that requires knowing this project: which
// flags the binaries take, which metrics they export, which runbooks exist.
package deploy_test

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// repoRoot walks up from the test's directory until it finds go.mod. The tests
// read files across the whole tree, and `go test` runs them in their own package
// directory.
func repoRoot(tb testing.TB) string {
	tb.Helper()
	dir, err := os.Getwd()
	if err != nil {
		tb.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			tb.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}

// ---------------------------------------------------------------------------
// The flags a binary actually has.
// ---------------------------------------------------------------------------

// flagNames extracts every flag registered in a main package by reading its AST.
//
// The alternative — build the binary and parse `-help` — costs a compile and a
// process per run and gives a worse answer, because a flag registered behind an
// `if` still appears in the source. Every flag in this project is registered with
// a string literal, which is what makes this work; a flag whose name were computed
// would silently vanish from this set, so the test below also asserts the set is
// implausibly large before trusting it.
func flagNames(tb testing.TB, dir string) map[string]bool {
	tb.Helper()

	// parser.ParseDir would be the obvious call, and it is deprecated: it ignores
	// build tags when deciding which files belong to a package. That does not
	// matter here — no flag is registered behind a build tag — but the files are
	// easy enough to enumerate, and a deprecated call in a test is a deprecated
	// call somebody copies.
	entries, err := os.ReadDir(dir)
	if err != nil {
		tb.Fatalf("reading %s: %v", dir, err)
	}

	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, 0)
		if err != nil {
			tb.Fatalf("parsing %s: %v", e.Name(), err)
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		tb.Fatalf("no Go files in %s", dir)
	}

	// flag.String("name", ...) and flag.StringVar(&x, "name", ...) put the name in
	// argument 0 and 1 respectively.
	nameArg := map[string]int{
		"Bool": 0, "Int": 0, "Int64": 0, "Uint": 0, "Uint64": 0,
		"String": 0, "Float64": 0, "Duration": 0, "Func": 0,
		"BoolVar": 1, "IntVar": 1, "Int64Var": 1, "UintVar": 1, "Uint64Var": 1,
		"StringVar": 1, "Float64Var": 1, "DurationVar": 1,
	}

	names := make(map[string]bool)
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := sel.X.(*ast.Ident)
			if !ok || ident.Name != "flag" {
				return true
			}
			idx, ok := nameArg[sel.Sel.Name]
			if !ok || idx >= len(call.Args) {
				return true
			}
			lit, ok := call.Args[idx].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				tb.Errorf("%s: a flag name is not a string literal, so this test cannot see it: %s",
					fset.Position(call.Pos()), sel.Sel.Name)
				return true
			}
			names[strings.Trim(lit.Value, `"`)] = true
			return true
		})
	}
	return names
}

// TestManifestFlagsExist is the test that stops a manifest from outliving the
// flag it passes.
//
// A container that starts with an unknown flag does not start at all: Go's flag
// package prints usage and exits 2. In Kubernetes that is a CrashLoopBackOff whose
// only clue is a usage message in a log nobody is tailing. This test moves that
// failure to the pull request that renamed the flag.
func TestManifestFlagsExist(t *testing.T) {
	root := repoRoot(t)

	serverFlags := flagNames(t, filepath.Join(root, "cmd", "server"))
	agentFlags := flagNames(t, filepath.Join(root, "cmd", "agent"))

	// A sanity floor. If the AST walk started matching nothing — a refactor moved
	// the flags behind a helper, say — every assertion below would pass vacuously.
	if len(serverFlags) < 20 {
		t.Fatalf("found only %d server flags; the AST walk is no longer seeing them", len(serverFlags))
	}
	if len(agentFlags) < 15 {
		t.Fatalf("found only %d agent flags; the AST walk is no longer seeing them", len(agentFlags))
	}

	// Every -flag= token that appears anywhere in the deployment artefacts, mapped
	// to the binary it is passed to.
	flagRe := regexp.MustCompile(`-{1,2}([a-z][a-z0-9-]*)(=|$|\s)`)

	type source struct {
		path  string
		which map[string]bool
		bin   string
	}

	// Manifests and compose pass flags in args/command lists. Rather than model
	// every schema, walk the YAML for containers and read their args.
	var sources []source
	for _, spec := range collectContainerArgs(t, root) {
		which, bin := serverFlags, "server"
		if strings.Contains(spec.image, "agent") {
			which, bin = agentFlags, "agent"
		}
		for _, arg := range spec.args {
			m := flagRe.FindStringSubmatch(arg)
			if m == nil {
				continue
			}
			name := m[1]
			if !which[name] {
				t.Errorf("%s: container %q passes -%s, which %s does not define.\n"+
					"A container that starts with an unknown flag exits 2 with a usage message.",
					spec.path, spec.name, name, bin)
			}
		}
		sources = append(sources, source{path: spec.path, which: which, bin: bin})
	}

	if len(sources) == 0 {
		t.Fatal("no containers found in the deployment artefacts; this test is checking nothing")
	}
	t.Logf("checked the arguments of %d containers against %d server and %d agent flags",
		len(sources), len(serverFlags), len(agentFlags))
}

// containerSpec is the little of a container this test needs.
type containerSpec struct {
	path  string
	name  string
	image string
	args  []string
}

// collectContainerArgs reads every YAML document under deploy/ that is not a Helm
// template (those contain Go template syntax and are not YAML until rendered) and
// pulls out the containers.
func collectContainerArgs(tb testing.TB, root string) []containerSpec {
	tb.Helper()

	var out []containerSpec
	deployDir := filepath.Join(root, "deploy")

	err := filepath.WalkDir(deployDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Helm templates are not YAML until helm has rendered them. CI renders
			// the chart and runs kubeconform over the result; this test reads the
			// artefacts that are already YAML.
			if d.Name() == "charts" {
				return filepath.SkipDir
			}
			return nil
		}
		if ext := filepath.Ext(path); ext != ".yaml" && ext != ".yml" {
			return nil
		}
		// Prometheus and Grafana config are YAML but describe no containers.
		if strings.Contains(path, "prometheus") || strings.Contains(path, "grafana") {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)

		dec := yaml.NewDecoder(strings.NewReader(string(data)))
		for {
			var doc map[string]any
			if err := dec.Decode(&doc); err != nil {
				break
			}
			out = append(out, containersIn(rel, doc)...)
		}
		return nil
	})
	if err != nil {
		tb.Fatalf("walking deploy/: %v", err)
	}
	return out
}

// containersIn finds containers in both shapes this repo uses: a Kubernetes
// workload's spec.template.spec.containers, and a compose file's services map.
func containersIn(path string, doc map[string]any) []containerSpec {
	var out []containerSpec

	// compose: services.<name>.{image,command}
	if services, ok := doc["services"].(map[string]any); ok {
		for name, raw := range services {
			svc, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			image, _ := svc["image"].(string)
			// Only our own services take our flags. prometheus and grafana take theirs.
			if !strings.Contains(image, "traceforge") {
				continue
			}
			out = append(out, containerSpec{
				path:  path,
				name:  name,
				image: image,
				args:  stringsIn(svc["command"]),
			})
		}
		return out
	}

	// kubernetes: spec.template.spec.containers[]
	for _, c := range podContainers(doc) {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		name, _ := cm["name"].(string)
		image, _ := cm["image"].(string)
		out = append(out, containerSpec{
			path:  path,
			name:  name,
			image: image,
			args:  append(stringsIn(cm["command"]), stringsIn(cm["args"])...),
		})
	}
	return out
}

func podContainers(doc map[string]any) []any {
	spec, ok := doc["spec"].(map[string]any)
	if !ok {
		return nil
	}
	tmpl, ok := spec["template"].(map[string]any)
	if !ok {
		return nil
	}
	podSpec, ok := tmpl["spec"].(map[string]any)
	if !ok {
		return nil
	}
	cs, _ := podSpec["containers"].([]any)
	return cs
}

func stringsIn(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		if s, ok := it.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// The metrics a binary actually exports.
// ---------------------------------------------------------------------------

// exportedMetrics reads every metric name out of the Go source. The names are
// string literals in the gatherers, so a grep-with-a-parser finds them all; a
// name assembled at runtime would be missed, and the floor check below catches
// the day that starts happening.
func exportedMetrics(tb testing.TB, root string) map[string]bool {
	tb.Helper()

	nameRe := regexp.MustCompile(`"(traceforge_[a-z0-9_]+)"`)
	names := make(map[string]bool)

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "deploy", "docs", "test", "web", "bin":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range nameRe.FindAllStringSubmatch(string(data), -1) {
			names[m[1]] = true
		}
		return nil
	})
	if err != nil {
		tb.Fatalf("scanning for metric names: %v", err)
	}
	return names
}

// knownMetric reports whether name is a series a scrape can actually produce.
// A histogram family named X yields X_bucket, X_sum and X_count on the wire; only
// X appears in the source.
func knownMetric(names map[string]bool, name string) bool {
	if names[name] {
		return true
	}
	for _, suffix := range []string{"_bucket", "_sum", "_count"} {
		if base, ok := strings.CutSuffix(name, suffix); ok && names[base] {
			return true
		}
	}
	return false
}

// TestDashboardsReferenceRealMetrics is the test that stops a dashboard from
// quietly showing nothing.
//
// A panel whose expression names a metric that no longer exists does not error.
// It renders an empty graph, which is indistinguishable from a healthy system,
// and it stays that way until an incident makes somebody look at it closely. That
// is the worst possible time to discover it.
func TestDashboardsReferenceRealMetrics(t *testing.T) {
	root := repoRoot(t)
	known := exportedMetrics(t, root)
	if len(known) < 20 {
		t.Fatalf("found only %d exported metric names in the source; the scan is broken", len(known))
	}

	dashboards, err := filepath.Glob(filepath.Join(root, "deploy", "dashboards", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(dashboards) == 0 {
		t.Fatal("no dashboards found; this test is checking nothing")
	}

	tokenRe := regexp.MustCompile(`traceforge_[a-z0-9_]+`)

	for _, path := range dashboards {
		rel, _ := filepath.Rel(root, path)
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var dash map[string]any
			if err := json.Unmarshal(data, &dash); err != nil {
				t.Fatalf("%s is not valid JSON: %v", rel, err)
			}
			if uid, _ := dash["uid"].(string); uid == "" {
				t.Errorf("%s has no uid; provisioning would create a new dashboard on every restart", rel)
			}
			if title, _ := dash["title"].(string); title == "" {
				t.Errorf("%s has no title", rel)
			}

			exprs := collectExprs(dash)
			if len(exprs) == 0 {
				t.Fatalf("%s has no panel targets; this test is checking nothing", rel)
			}

			for _, expr := range exprs {
				for _, tok := range tokenRe.FindAllString(expr, -1) {
					if !knownMetric(known, tok) {
						t.Errorf("%s: panel references %q, which no binary exports.\n"+
							"  expr: %s\n"+
							"  A panel with a misspelled metric shows an empty graph, not an error.",
							rel, tok, expr)
					}
				}
			}
			t.Logf("%s: %d expressions, all metric names exist", rel, len(exprs))
		})
	}
}

// collectExprs walks an arbitrary decoded dashboard for every "expr" string. The
// panel tree nests (rows contain panels), so the walk is generic rather than
// following a schema Grafana is free to change.
func collectExprs(v any) []string {
	var out []string
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if k == "expr" {
				if s, ok := child.(string); ok && s != "" {
					out = append(out, s)
				}
				continue
			}
			out = append(out, collectExprs(child)...)
		}
	case []any:
		for _, child := range t {
			out = append(out, collectExprs(child)...)
		}
	}
	return out
}

// TestAlertRulesReferenceRealMetrics does for alerts what the test above does for
// dashboards. The failure mode is worse here: an alert on a metric that does not
// exist never fires, so the thing it was written to catch happens unobserved.
func TestAlertRulesReferenceRealMetrics(t *testing.T) {
	root := repoRoot(t)
	known := exportedMetrics(t, root)

	path := filepath.Join(root, "deploy", "prometheus", "alerts.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var doc struct {
		Groups []struct {
			Name  string `yaml:"name"`
			Rules []struct {
				Alert       string            `yaml:"alert"`
				Expr        string            `yaml:"expr"`
				Annotations map[string]string `yaml:"annotations"`
				Labels      map[string]string `yaml:"labels"`
			} `yaml:"rules"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("alerts.yml: %v", err)
	}

	tokenRe := regexp.MustCompile(`traceforge_[a-z0-9_]+`)
	count := 0
	for _, g := range doc.Groups {
		for _, r := range g.Rules {
			count++
			for _, tok := range tokenRe.FindAllString(r.Expr, -1) {
				if !knownMetric(known, tok) {
					t.Errorf("alert %s references %q, which no binary exports.\n"+
						"An alert on a metric that does not exist never fires.", r.Alert, tok)
				}
			}
			if r.Labels["severity"] == "" {
				t.Errorf("alert %s has no severity label; routing cannot page anyone for it", r.Alert)
			}
			if r.Annotations["summary"] == "" {
				t.Errorf("alert %s has no summary", r.Alert)
			}
		}
	}
	if count == 0 {
		t.Fatal("no alert rules found; this test is checking nothing")
	}
	t.Logf("checked %d alert rules", count)
}

// TestEveryAlertHasARunbookThatExists.
//
// A runbook_url that 404s is worse than no link at all: it costs the on-call
// engineer the ninety seconds it takes to discover that the answer they were
// promised is not there.
func TestEveryAlertHasARunbookThatExists(t *testing.T) {
	root := repoRoot(t)

	data, err := os.ReadFile(filepath.Join(root, "deploy", "prometheus", "alerts.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Groups []struct {
			Rules []struct {
				Alert       string            `yaml:"alert"`
				Annotations map[string]string `yaml:"annotations"`
			} `yaml:"rules"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}

	referenced := map[string]bool{}
	for _, g := range doc.Groups {
		for _, r := range g.Rules {
			url := r.Annotations["runbook_url"]
			if url == "" {
				t.Errorf("alert %s has no runbook_url. An alert without a runbook is a page "+
					"without an answer.", r.Alert)
				continue
			}
			file := filepath.Base(url)
			// The runbook is named after the alert. Anything else is a link that
			// rots the first time somebody renames one of the two.
			if want := r.Alert + ".md"; file != want {
				t.Errorf("alert %s links %s; the convention is %s", r.Alert, file, want)
			}
			path := filepath.Join(root, "docs", "runbooks", file)
			if _, err := os.Stat(path); err != nil {
				t.Errorf("alert %s links a runbook that does not exist: docs/runbooks/%s", r.Alert, file)
				continue
			}
			referenced[file] = true

			assertRunbookStructure(t, path)
		}
	}

	// And the other direction: a runbook nobody links is a runbook nobody will
	// find, and probably an alert somebody deleted.
	entries, err := os.ReadDir(filepath.Join(root, "docs", "runbooks"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || e.Name() == "README.md" || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		if !referenced[e.Name()] {
			t.Errorf("docs/runbooks/%s is linked by no alert", e.Name())
		}
	}
	if len(referenced) == 0 {
		t.Fatal("no runbooks referenced; this test is checking nothing")
	}
	t.Logf("checked %d runbooks", len(referenced))
}

// assertRunbookStructure checks the sections an on-call engineer scrolls for. The
// order matters: at 03:00 nobody reads a runbook, they scan it.
func assertRunbookStructure(tb testing.TB, path string) {
	tb.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		tb.Fatal(err)
	}
	body := string(data)
	want := []string{
		"## What fired",
		"## Impact",
		"## Diagnose",
		"## Likely causes",
		"## Mitigate",
		"## Escalate",
		"## Why this alert exists",
	}
	var missing []string
	pos := -1
	for _, section := range want {
		i := strings.Index(body, section)
		if i < 0 {
			missing = append(missing, section)
			continue
		}
		if i < pos {
			tb.Errorf("%s: section %q appears out of order", filepath.Base(path), section)
		}
		pos = i
	}
	if len(missing) > 0 {
		tb.Errorf("%s is missing sections: %s", filepath.Base(path), strings.Join(missing, ", "))
	}
	// A runbook that says TODO is a runbook that has not been written.
	for _, marker := range []string{"TODO", "FIXME", "TBD", "<placeholder>"} {
		if strings.Contains(body, marker) {
			tb.Errorf("%s contains %q", filepath.Base(path), marker)
		}
	}
}

// ---------------------------------------------------------------------------
// The security posture of the manifests.
// ---------------------------------------------------------------------------

// TestComposeServicesAreHardened checks the compose file for the settings that a
// Kubernetes PodSecurity policy would enforce and docker-compose will not.
//
// The Helm chart's equivalents are checked in CI by rendering it and running the
// same assertions over the output; this covers the file a developer actually runs.
func TestComposeServicesAreHardened(t *testing.T) {
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "deploy", "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Services map[string]struct {
			Image     string   `yaml:"image"`
			ReadOnly  bool     `yaml:"read_only"`
			CapDrop   []string `yaml:"cap_drop"`
			SecurityO []string `yaml:"security_opt"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}

	ours := 0
	for name, svc := range doc.Services {
		if !strings.Contains(svc.Image, "traceforge") {
			continue
		}
		ours++
		if !svc.ReadOnly {
			t.Errorf("service %s does not set read_only; an attacker with RCE can persist", name)
		}
		if !slices.Contains(svc.CapDrop, "ALL") {
			t.Errorf("service %s does not drop ALL capabilities", name)
		}
		if !slices.Contains(svc.SecurityO, "no-new-privileges:true") {
			t.Errorf("service %s allows privilege escalation", name)
		}
		if strings.HasSuffix(svc.Image, ":latest") {
			t.Errorf("service %s pins :latest; nobody can say what is running", name)
		}
	}
	if ours == 0 {
		t.Fatal("no traceforge services in compose.yaml; this test is checking nothing")
	}
}

// TestPrometheusScrapesTheTelemetryPortOnly.
//
// /metrics lives on the telemetry listener, never on the API port. A scrape
// config aimed at 8080 would find nothing and report the target as down, which is
// a confusing way to learn where an endpoint lives.
func TestPrometheusScrapesTheTelemetryPortOnly(t *testing.T) {
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "deploy", "prometheus", "prometheus.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		ScrapeConfigs []struct {
			JobName       string `yaml:"job_name"`
			StaticConfigs []struct {
				Targets []string `yaml:"targets"`
			} `yaml:"static_configs"`
		} `yaml:"scrape_configs"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}

	// The defaults the binaries ship with. If these change, the scrape config must.
	wantPort := map[string]string{
		"traceforge-server": "9091",
		"traceforge-agent":  "9101",
	}
	seen := map[string]bool{}
	for _, sc := range doc.ScrapeConfigs {
		port, ok := wantPort[sc.JobName]
		if !ok {
			continue
		}
		seen[sc.JobName] = true
		for _, cfg := range sc.StaticConfigs {
			for _, target := range cfg.Targets {
				if !strings.HasSuffix(target, ":"+port) {
					t.Errorf("job %s scrapes %s, but /metrics is served on port %s", sc.JobName, target, port)
				}
			}
		}
	}
	for job := range wantPort {
		if !seen[job] {
			t.Errorf("prometheus.yml has no scrape job for %s", job)
		}
	}
}

// TestTelemetryDefaultsMatchTheDeployment ties the numbers in the YAML back to the
// defaults in the Go source, which is the only place they are authoritative.
func TestTelemetryDefaultsMatchTheDeployment(t *testing.T) {
	root := repoRoot(t)

	checks := []struct {
		file string
		want string
		why  string
	}{
		{filepath.Join("cmd", "server", "main.go"), `envString("TELEMETRY_ADDR", ":9091")`,
			"prometheus.yml and the chart both scrape :9091"},
		{filepath.Join("cmd", "agent", "main.go"), `envString("AGENT_TELEMETRY_ADDR", ":9101")`,
			"prometheus.yml scrapes the agent on :9101"},
	}
	for _, c := range checks {
		data, err := os.ReadFile(filepath.Join(root, c.file))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), c.want) {
			t.Errorf("%s no longer contains %s, but %s", c.file, c.want, c.why)
		}
	}
}

// ---------------------------------------------------------------------------
// The security posture of the rendered Kubernetes manifests.
//
// deploy/k8s/traceforge.yaml is generated from the Helm chart by `make manifests`
// and committed. Committing generated YAML is usually a mistake; here it buys two
// things. An operator without helm can apply it, and — the reason it is checked in
// rather than gitignored — the assertions below get to run in the ordinary unit
// suite, on the exact bytes that reach a cluster, with no helm on the machine.
//
// CI regenerates it and fails on a diff, so the chart stays the source of truth.
// ---------------------------------------------------------------------------

// workload is the part of a Kubernetes workload these tests care about.
type workload struct {
	kind       string
	name       string
	podSpec    map[string]any
	podSecCtx  map[string]any
	containers []map[string]any
}

func renderedWorkloads(tb testing.TB, root string) []workload {
	tb.Helper()

	data, err := os.ReadFile(filepath.Join(root, "deploy", "k8s", "traceforge.yaml"))
	if err != nil {
		tb.Fatalf("deploy/k8s/traceforge.yaml is missing; run `make manifests`: %v", err)
	}

	var out []workload
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			break
		}
		kind, _ := doc["kind"].(string)
		switch kind {
		case "Deployment", "StatefulSet", "DaemonSet":
		default:
			continue
		}
		meta, _ := doc["metadata"].(map[string]any)
		name, _ := meta["name"].(string)

		spec, _ := doc["spec"].(map[string]any)
		tmpl, _ := spec["template"].(map[string]any)
		podSpec, _ := tmpl["spec"].(map[string]any)
		if podSpec == nil {
			tb.Fatalf("%s %s has no pod spec", kind, name)
		}
		secCtx, _ := podSpec["securityContext"].(map[string]any)

		var containers []map[string]any
		raw, _ := podSpec["containers"].([]any)
		for _, c := range raw {
			if cm, ok := c.(map[string]any); ok {
				containers = append(containers, cm)
			}
		}
		out = append(out, workload{
			kind: kind, name: name, podSpec: podSpec, podSecCtx: secCtx, containers: containers,
		})
	}
	return out
}

// TestRenderedManifestsAreHardened.
//
// Each of these has a specific attacker in mind, and none of them is theoretical:
//
//   - runAsNonRoot: a container escape from a root process is a root shell on the
//     node. From uid 65532 it is a shell as nobody.
//   - readOnlyRootFilesystem: an attacker with remote code execution cannot write
//     a backdoor, a miner, or a second stage anywhere the kubelet will keep.
//   - drop ALL capabilities: this server binds ports above 1024 and reads no raw
//     sockets. It needs none of them, so it gets none.
//   - allowPrivilegeEscalation: false stops a setuid binary in the image — there
//     are none, and this makes sure a future base image cannot add one.
//   - seccompProfile RuntimeDefault: the syscall filter that turns most kernel
//     exploits into EPERM.
//
// The agent with packet capture is the one exception, and the test states it
// explicitly rather than letting it slip through a loop.
func TestRenderedManifestsAreHardened(t *testing.T) {
	root := repoRoot(t)
	workloads := renderedWorkloads(t, root)
	if len(workloads) < 2 {
		t.Fatalf("found %d workloads in the rendered manifests; expected at least the "+
			"server StatefulSet and the agent DaemonSet", len(workloads))
	}

	for _, w := range workloads {
		t.Run(w.kind+"/"+w.name, func(t *testing.T) {
			// Pod-level.
			if v, _ := w.podSecCtx["runAsNonRoot"].(bool); !v {
				t.Errorf("pod securityContext.runAsNonRoot is not true")
			}
			if uid, ok := w.podSecCtx["runAsUser"].(int); !ok || uid == 0 {
				t.Errorf("pod securityContext.runAsUser = %v, want a non-zero uid", w.podSecCtx["runAsUser"])
			}
			seccomp, _ := w.podSecCtx["seccompProfile"].(map[string]any)
			if typ, _ := seccomp["type"].(string); typ != "RuntimeDefault" {
				t.Errorf("pod seccompProfile.type = %q, want RuntimeDefault", typ)
			}

			if len(w.containers) == 0 {
				t.Fatal("no containers")
			}
			for _, c := range w.containers {
				name, _ := c["name"].(string)
				assertContainerHardened(t, name, c)
				assertContainerHasProbes(t, name, c)
				assertContainerHasResources(t, name, c)
				assertImageIsPinned(t, name, c)
			}
		})
	}
}

func assertContainerHardened(tb testing.TB, name string, c map[string]any) {
	tb.Helper()
	sec, _ := c["securityContext"].(map[string]any)
	if sec == nil {
		tb.Errorf("container %s has no securityContext", name)
		return
	}
	if v, ok := sec["allowPrivilegeEscalation"].(bool); !ok || v {
		tb.Errorf("container %s allows privilege escalation", name)
	}
	if v, ok := sec["readOnlyRootFilesystem"].(bool); !ok || !v {
		tb.Errorf("container %s has a writable root filesystem; an attacker with RCE can persist", name)
	}
	caps, _ := sec["capabilities"].(map[string]any)
	drop, _ := caps["drop"].([]any)
	dropsAll := false
	for _, d := range drop {
		if s, _ := d.(string); s == "ALL" {
			dropsAll = true
		}
	}
	if !dropsAll {
		tb.Errorf("container %s does not drop ALL capabilities", name)
	}
}

// assertContainerHasProbes also checks that each probe's port is one the container
// actually declares. A probe pointed at a port nothing listens on fails for ever,
// and in the case of a liveness probe it restarts the pod for ever.
func assertContainerHasProbes(tb testing.TB, name string, c map[string]any) {
	tb.Helper()

	declared := map[string]bool{}
	ports, _ := c["ports"].([]any)
	for _, p := range ports {
		pm, _ := p.(map[string]any)
		if n, ok := pm["name"].(string); ok {
			declared[n] = true
		}
	}

	for _, probe := range []struct{ key, path string }{
		{"livenessProbe", "/healthz"},
		{"readinessProbe", "/readyz"},
		{"startupProbe", "/startupz"},
	} {
		p, _ := c[probe.key].(map[string]any)
		if p == nil {
			tb.Errorf("container %s has no %s", name, probe.key)
			continue
		}
		hg, _ := p["httpGet"].(map[string]any)
		if path, _ := hg["path"].(string); path != probe.path {
			tb.Errorf("container %s %s probes %q, want %q", name, probe.key, path, probe.path)
		}
		port, _ := hg["port"].(string)
		if port == "" {
			tb.Errorf("container %s %s has no named port", name, probe.key)
			continue
		}
		if !declared[port] {
			tb.Errorf("container %s %s probes port %q, which is not one of its containerPorts %v.\n"+
				"A liveness probe on a port nothing listens on restarts the pod for ever.",
				name, probe.key, port, declared)
		}
	}
}

// assertContainerHasResources: without requests the scheduler cannot place the pod
// honestly; without a memory limit one leaking pod takes the node down with it.
func assertContainerHasResources(tb testing.TB, name string, c map[string]any) {
	tb.Helper()
	res, _ := c["resources"].(map[string]any)
	for _, side := range []string{"requests", "limits"} {
		m, _ := res[side].(map[string]any)
		if m == nil {
			tb.Errorf("container %s has no resources.%s", name, side)
			continue
		}
		for _, key := range []string{"cpu", "memory"} {
			if _, ok := m[key]; !ok {
				tb.Errorf("container %s has no resources.%s.%s", name, side, key)
			}
		}
	}
}

// assertImageIsPinned. `:latest` means nobody can say what is running, and a
// rollback restores the tag rather than the bytes.
func assertImageIsPinned(tb testing.TB, name string, c map[string]any) {
	tb.Helper()
	image, _ := c["image"].(string)
	if image == "" {
		tb.Errorf("container %s has no image", name)
		return
	}
	tag := ""
	if i := strings.LastIndexByte(image, ':'); i >= 0 && !strings.Contains(image[i:], "/") {
		tag = image[i+1:]
	}
	switch tag {
	case "":
		tb.Errorf("container %s image %q has no tag, which means :latest", name, image)
	case "latest":
		tb.Errorf("container %s image %q is pinned to :latest", name, image)
	}
}

// TestShutdownBudgetsAreConsistent.
//
// Three numbers have to be ordered or the graceful shutdown is a fiction:
//
//	shutdown-delay  <  shutdown-timeout  <  terminationGracePeriodSeconds
//
// The delay is how long the server keeps serving after failing readiness, so the
// load balancer can notice. The timeout is the process's own deadline for the
// whole drain. The grace period is when the kubelet stops asking and sends
// SIGKILL. Get the order wrong and the pod is killed mid-drain, in the middle of
// an fsync, on every single deploy.
func TestShutdownBudgetsAreConsistent(t *testing.T) {
	root := repoRoot(t)

	// The `continue` below skips any container without the flags, which is right
	// for the agent and catastrophic for the server: delete `-shutdown-timeout`
	// from the manifest and this test would assert nothing, in silence, for ever.
	// Counting what was actually checked is the difference between a test and a
	// decoration.
	checked := 0

	for _, w := range renderedWorkloads(t, root) {
		grace, ok := w.podSpec["terminationGracePeriodSeconds"].(int)
		if !ok {
			t.Errorf("%s/%s does not set terminationGracePeriodSeconds", w.kind, w.name)
			continue
		}

		for _, c := range w.containers {
			args := stringsIn(c["args"])
			delay := argDuration(t, args, "-shutdown-delay")
			timeout := argDuration(t, args, "-shutdown-timeout")
			if timeout == 0 {
				continue // the agent takes neither flag
			}
			checked++

			if delay >= timeout {
				t.Errorf("%s/%s: -shutdown-delay=%v is not less than -shutdown-timeout=%v; "+
					"the drain would begin after its own deadline", w.kind, w.name, delay, timeout)
			}
			if float64(grace) <= timeout {
				t.Errorf("%s/%s: terminationGracePeriodSeconds=%d is not greater than "+
					"-shutdown-timeout=%vs; the kubelet would SIGKILL mid-drain",
					w.kind, w.name, grace, timeout)
			}
		}
	}

	if checked == 0 {
		t.Fatal("no container in the rendered manifests carries -shutdown-timeout, " +
			"so this test is checking nothing; the drain ordering it protects is unenforced")
	}
}

// argDuration finds `-name=<duration>` in args and returns it in seconds, or 0.
func argDuration(tb testing.TB, args []string, name string) float64 {
	tb.Helper()
	for _, a := range args {
		v, ok := strings.CutPrefix(a, name+"=")
		if !ok {
			continue
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			tb.Errorf("%s=%q is not a duration: %v", name, v, err)
			return 0
		}
		return d.Seconds()
	}
	return 0
}
