package runner

import (
	"github.com/steadyspacecorp/openroutines/internal/knowledge"
	"github.com/steadyspacecorp/openroutines/internal/routine"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// teamwork: off is enforced at import, not just instructed: a staged change
// to events.md is discarded (worktree copy wins) while the rest imports.
func TestImportKnowledgeEnforcesEventsOptOut(t *testing.T) {
	setup := func(t *testing.T) (string, *AttemptWorkspace) {
		t.Helper()
		dir := t.TempDir()
		wt := filepath.Join(dir, knowledge.Dir)
		if err := os.MkdirAll(wt, 0o755); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(wt, "events.md"), []byte("base\n"), 0o644)
		os.WriteFile(filepath.Join(wt, "tasks.md"), []byte("none\n"), 0o644)
		staging := &AttemptWorkspace{KnowledgeDir: t.TempDir(), BaseDir: t.TempDir()}
		os.WriteFile(filepath.Join(staging.BaseDir, "events.md"), []byte("base\n"), 0o644)
		os.WriteFile(filepath.Join(staging.BaseDir, "tasks.md"), []byte("none\n"), 0o644)
		os.WriteFile(filepath.Join(staging.KnowledgeDir, "events.md"), []byte("base\n- sneaky event\n"), 0o644)
		os.WriteFile(filepath.Join(staging.KnowledgeDir, "tasks.md"), []byte("- [ ] real work\n"), 0o644)
		return dir, staging
	}

	dir, staging := setup(t)
	r := &routine.Routine{Name: "quiet", Frontmatter: routine.Frontmatter{Teamwork: routine.TeamworkOff}}
	discarded, _, err := importKnowledge(dir, r, staging)
	if err != nil || !discarded {
		t.Fatalf("discarded=%v err=%v, want true nil", discarded, err)
	}
	wt := filepath.Join(dir, knowledge.Dir)
	if got, _ := os.ReadFile(filepath.Join(wt, "events.md")); string(got) != "base\n" {
		t.Fatalf("events.md = %q, want staged change discarded", got)
	}
	if got, _ := os.ReadFile(filepath.Join(wt, "tasks.md")); string(got) != "- [ ] real work\n" {
		t.Fatalf("tasks.md = %q, want staged change imported", got)
	}

	dir, staging = setup(t)
	r = &routine.Routine{Name: "loud", Frontmatter: routine.Frontmatter{}}
	discarded, _, err = importKnowledge(dir, r, staging)
	if err != nil || discarded {
		t.Fatalf("discarded=%v err=%v, want false nil", discarded, err)
	}
	wt = filepath.Join(dir, knowledge.Dir)
	if got, _ := os.ReadFile(filepath.Join(wt, "events.md")); string(got) != "base\n- sneaky event\n" {
		t.Fatalf("events.md = %q, want staged change imported for a recording routine", got)
	}
}

// settleFixture builds a real agent repo with a materialized knowledge worktree:
// Settle's commit step needs actual git.
func settleFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("git", "init", "-q", "-b", "main", ".")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if err := knowledge.NewStore(dir).Ensure(); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A new reporting routine's change set is empty by construction. Its first
// successful run must still persist the current boundary, or every later run
// is another "first run" that skips forward and receives nothing forever.
func TestSettleBootstrapsAnEmptyConsumerCursor(t *testing.T) {
	dir := settleFixture(t)
	store := knowledge.NewStore(dir)
	through, err := store.Head()
	if err != nil {
		t.Fatal(err)
	}
	stage := func(first bool, boundary string) *AttemptWorkspace {
		s := &AttemptWorkspace{
			KnowledgeDir: t.TempDir(),
			BaseDir:      t.TempDir(),
			root:         t.TempDir(),
			Delivery:     DeliveryBoundary{Through: boundary, FirstRun: first},
		}
		if err := store.Snapshot(s.BaseDir); err != nil {
			t.Fatal(err)
		}
		if err := knowledge.CloneTree(s.BaseDir, s.KnowledgeDir); err != nil {
			t.Fatal(err)
		}
		return s
	}
	r := &routine.Routine{Name: "slack-report", Frontmatter: routine.Frontmatter{Reports: true}}
	if _, err := Settle(dir, r, stage(true, through), &AttemptResult{Outcome: Completed}, Attempt{RunID: "run_first", Number: 1}, "", nil); err != nil {
		t.Fatal(err)
	}
	cursor, err := store.LoadCursor(r.Name)
	if err != nil || cursor == nil || cursor.ConsumedThrough != through || cursor.ByRun != "run_first" {
		t.Fatalf("bootstrap cursor = %+v, err = %v; want %s by run_first", cursor, err, through)
	}

	// Once initialized, successful completion alone must not consume real
	// pending changes: the explicit marker remains the delivery receipt.
	newBoundary, err := store.Head()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Settle(dir, r, stage(false, newBoundary), &AttemptResult{Outcome: Completed}, Attempt{RunID: "run_empty", Number: 1}, "", nil); err != nil {
		t.Fatal(err)
	}
	cursor, err = store.LoadCursor(r.Name)
	if err != nil || cursor.ConsumedThrough != through || cursor.ByRun != "run_first" {
		t.Fatalf("cursor advanced without marker: %+v, err = %v", cursor, err)
	}
}

// A completed attempt whose staged knowledge is rejected settles as crashed --
// in the returned outcome, the failure event, the run record, and the
// settlement commit alike. The run record saying "completed" while the run
// was reported crashed is the divergence Settle exists to prevent.
func TestSettleRecordsRejectedImportAsCrashed(t *testing.T) {
	dir := settleFixture(t)
	staging := &AttemptWorkspace{KnowledgeDir: t.TempDir()}
	os.WriteFile(filepath.Join(staging.KnowledgeDir, ".gitignore"), []byte("x"), 0o644)

	r := &routine.Routine{Name: "x", Frontmatter: routine.Frontmatter{}}
	settlement, err := Settle(dir, r, staging, &AttemptResult{Outcome: Completed}, Attempt{RunID: "run_reject", Number: 1}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if settlement.Outcome != Crashed || !strings.Contains(settlement.Detail, "knowledge rejected") {
		t.Fatalf("settlement = %+v, want crashed with knowledge-rejected detail", settlement)
	}
	if settlement.Commit == "" {
		t.Fatal("settlement should have committed the record and event")
	}
	wt := filepath.Join(dir, knowledge.Dir)
	records, _ := os.ReadFile(filepath.Join(wt, "runs.jsonl"))
	if !strings.Contains(string(records), `"outcome":"crashed"`) {
		t.Fatalf("run record should carry the settled outcome: %s", records)
	}
	events, _ := os.ReadFile(filepath.Join(wt, "events.md"))
	if !strings.Contains(string(events), "run_reject attempt_01) knowledge rejected") {
		t.Fatalf("failure event missing: %s", events)
	}
}

// A clean completion imports staged knowledge, runs the caller's stage hook
// before the settlement commit (so its writes ride along), and commits.
func TestSettleImportsAndCommitsCompletedRun(t *testing.T) {
	dir := settleFixture(t)
	store := knowledge.NewStore(dir)
	staging := &AttemptWorkspace{KnowledgeDir: t.TempDir()}
	if err := store.Snapshot(staging.KnowledgeDir); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(staging.KnowledgeDir, "ledgers", "x.md"), []byte("worked\n"), 0o644)

	staged := false
	r := &routine.Routine{Name: "x", Frontmatter: routine.Frontmatter{}}
	settlement, err := Settle(dir, r, staging, &AttemptResult{Outcome: Completed}, Attempt{RunID: "run_ok", Number: 1}, "", func(fin *Settlement) {
		staged = fin.Outcome == Completed
		os.WriteFile(filepath.Join(store.StateDir(), "x.json"), []byte("{}\n"), 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	if settlement.Outcome != Completed || settlement.Detail != "" || settlement.Commit == "" {
		t.Fatalf("settlement = %+v, want clean completion with a commit", settlement)
	}
	if !staged {
		t.Fatal("stage hook should see the settled outcome")
	}
	wt := filepath.Join(dir, knowledge.Dir)
	if got, _ := os.ReadFile(filepath.Join(wt, "ledgers", "x.md")); string(got) != "worked\n" {
		t.Fatalf("staged knowledge not imported: %q", got)
	}
	// The stage hook's write is inside the settlement commit, not left dirty.
	if changed, _ := exec.Command("git", "-C", wt, "status", "--porcelain").Output(); len(changed) != 0 {
		t.Fatalf("worktree dirty after settlement: %s", changed)
	}
	records, _ := os.ReadFile(filepath.Join(wt, "runs.jsonl"))
	if !strings.Contains(string(records), `"outcome":"completed"`) || !strings.Contains(string(records), `"manual":true`) {
		t.Fatalf("run record wrong: %s", records)
	}
}

func TestSettleDoesNotBookkeepWithoutARunRecord(t *testing.T) {
	dir := settleFixture(t)
	store := knowledge.NewStore(dir)
	staging := &AttemptWorkspace{KnowledgeDir: t.TempDir()}
	if err := store.Snapshot(staging.KnowledgeDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(store.Worktree(), "runs.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}

	bookkept := false
	r := &routine.Routine{Name: "x", Frontmatter: routine.Frontmatter{}}
	_, err := Settle(dir, r, staging, &AttemptResult{Outcome: Completed}, Attempt{RunID: "run_unrecorded", Number: 1}, "", func(*Settlement) {
		bookkept = true
	})
	if err == nil {
		t.Fatal("Settle succeeded without writing a run record")
	}
	if bookkept {
		t.Fatal("scheduling bookkeeping ran before the run record was written")
	}
}

func TestConsumeMarkerLivesInStagedKnowledge(t *testing.T) {
	dir := t.TempDir()
	wt := filepath.Join(dir, knowledge.Dir)
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	staging := &AttemptWorkspace{KnowledgeDir: t.TempDir(), root: t.TempDir()}
	if staging.Consumed() {
		t.Fatal("Consumed() true with no marker anywhere")
	}
	// The sandbox leaves only staged knowledge writable: the marker there counts.
	os.WriteFile(filepath.Join(staging.KnowledgeDir, knowledge.ConsumeMarker), nil, 0o644)
	if !staging.Consumed() {
		t.Fatal("marker in staged knowledge not honored")
	}
	// It is a receipt for the runtime, not knowledge content: import drops it.
	r := &routine.Routine{Name: "report", Frontmatter: routine.Frontmatter{Reports: true}}
	if _, _, err := importKnowledge(dir, r, staging); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(wt, knowledge.ConsumeMarker)); !os.IsNotExist(err) {
		t.Fatal("consume marker imported into the knowledge worktree")
	}
	// Unsandboxed runs may still drop the marker at the workspace root.
	legacy := &AttemptWorkspace{KnowledgeDir: t.TempDir(), root: t.TempDir()}
	os.WriteFile(filepath.Join(legacy.root, knowledge.ConsumeMarker), nil, 0o644)
	if !legacy.Consumed() {
		t.Fatal("workspace-root marker no longer honored")
	}
}

// The record carries model, effort, and per-attempt tokens when present,
// and omits them -- never zeroes -- when the runtime didn't report.
func TestRecordJSONUsage(t *testing.T) {
	r := &routine.Routine{Name: "x"}
	attempt := Attempt{RunID: "run_1"}

	bare := recordJSON(r, attempt, &AttemptResult{Outcome: Completed})
	for _, absent := range []string{"tokens", "model", "effort", "cost_reported"} {
		if strings.Contains(bare, absent) {
			t.Fatalf("unreported %s must be omitted, got %s", absent, bare)
		}
	}

	res := &AttemptResult{Outcome: Completed, Model: "fake/model", Effort: "high",
		Usage: &Usage{Input: 100, Output: 20, Reasoning: 5, CacheRead: 7, CacheWrite: 3, CostReported: 0.01}}
	rec := recordJSON(r, attempt, res)
	for _, want := range []string{`"model":"fake/model"`, `"effort":"high"`, `"input":100`, `"output":20`,
		`"reasoning":5`, `"cache_read":7`, `"cache_write":3`, `"cost_reported":0.01`} {
		if !strings.Contains(rec, want) {
			t.Fatalf("record missing %s: %s", want, rec)
		}
	}
}
