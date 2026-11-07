package web

import (
	"strings"
	"testing"
)

const standardDiff = `diff --git a/internal/x.go b/internal/x.go
index 111..222 100644
--- a/internal/x.go
+++ b/internal/x.go
@@ -12,6 +12,8 @@ func main() {
 	a := 1
 	b := 2
+	c := 3
+	d := 4
 	return a + b
 }
@@ -30,3 +32,2 @@ func other() {
-	old()
 	keep()
`

func TestParseUnifiedDiffStandard(t *testing.T) {
	view := ParseUnifiedDiff(standardDiff, "internal/x.go")
	if view.Empty || view.Warning != "" {
		t.Fatalf("unexpected empty/warning: %+v", view)
	}
	if view.Added != 2 || view.Removed != 1 || view.HunkCount != 2 {
		t.Fatalf("stats = +%d -%d hunks=%d, want +2 -1 2", view.Added, view.Removed, view.HunkCount)
	}
	if len(view.Hunks) != 2 {
		t.Fatalf("hunks = %d", len(view.Hunks))
	}
	first := view.Hunks[0]
	if first.OldStart != 12 || first.OldCount != 6 || first.NewStart != 12 || first.NewCount != 8 {
		t.Fatalf("hunk header = %+v", first)
	}
	if !strings.Contains(first.Header, "func main") {
		t.Fatalf("hunk tail = %q", first.Header)
	}
	var added, removed, ctx int
	for _, line := range first.Lines {
		switch line.Kind {
		case "add":
			added++
			if line.NewNo == 0 || line.OldNo != 0 {
				t.Fatalf("add line numbers = %+v", line)
			}
		case "del":
			removed++
		case "ctx":
			ctx++
			if line.OldNo == 0 || line.NewNo == 0 {
				t.Fatalf("ctx line numbers = %+v", line)
			}
		}
	}
	if added != 2 || removed != 0 || ctx != 4 {
		t.Fatalf("first hunk lines add=%d del=%d ctx=%d", added, removed, ctx)
	}
	if first.Lines[0].OldNo != 12 || first.Lines[0].NewNo != 12 {
		t.Fatalf("first ctx numbers = %+v", first.Lines[0])
	}
}

func TestParseUnifiedDiffFence(t *testing.T) {
	fenced := "```diff\n" + standardDiff + "```\n"
	view := ParseUnifiedDiff(fenced, "x.go")
	if view.Added != 2 || view.Removed != 1 || len(view.Hunks) != 2 {
		t.Fatalf("fenced diff = +%d -%d hunks=%d", view.Added, view.Removed, len(view.Hunks))
	}
}

func TestParseUnifiedDiffNoHunks(t *testing.T) {
	view := ParseUnifiedDiff("not a diff\njust text", "x.go")
	if !view.Empty || view.Warning == "" {
		t.Fatalf("expected degraded empty view, got %+v", view)
	}
	if view.Raw != "not a diff\njust text" {
		t.Fatalf("raw = %q", view.Raw)
	}
	if _, _, hunks := DiffStats(view); hunks != 0 {
		t.Fatalf("hunks = %d", hunks)
	}
}

func TestParseUnifiedDiffEmpty(t *testing.T) {
	view := ParseUnifiedDiff("   \n", "x.go")
	if !view.Empty || view.Warning == "" {
		t.Fatalf("empty diff = %+v", view)
	}
}

func TestParseUnifiedDiffTruncatedHunks(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 60; i++ {
		b.WriteString("@@ -1,1 +1,1 @@\n")
		b.WriteString("-old\n+new\n")
	}
	view := ParseUnifiedDiff(b.String(), "x.go")
	if !view.Truncated {
		t.Fatal("expected truncated for >50 hunks")
	}
	if len(view.Hunks) != maxDiffHunks {
		t.Fatalf("hunks = %d, want %d", len(view.Hunks), maxDiffHunks)
	}
}

func TestParseUnifiedDiffNoNewlineNote(t *testing.T) {
	view := ParseUnifiedDiff("@@ -1,1 +1,1 @@\n-old\n+new\n\\ No newline at end of file\n", "x.go")
	if len(view.Hunks) != 1 {
		t.Fatalf("hunks = %d", len(view.Hunks))
	}
	last := view.Hunks[0].Lines[len(view.Hunks[0].Lines)-1]
	if last.Kind != "note" {
		t.Fatalf("last line = %+v", last)
	}
}

func TestParseUnifiedDiffStatsAndSummary(t *testing.T) {
	view := ParseUnifiedDiff(standardDiff, "x.go")
	if got := DiffSummary(view); got != "+2 −1" {
		t.Fatalf("summary = %q", got)
	}
	if added, removed, hunks := DiffStats(view); added != 2 || removed != 1 || hunks != 2 {
		t.Fatalf("stats = %d/%d/%d", added, removed, hunks)
	}
}
