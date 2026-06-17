package finishskill

import (
	"os"
	"testing"
)

func TestEnsureInstalled(t *testing.T) {
	home := t.TempDir()
	dest := InstallPath(home)

	// First call: writes the file fresh.
	wrote, err := EnsureInstalled(home)
	if err != nil {
		t.Fatalf("first EnsureInstalled: %v", err)
	}
	if !wrote {
		t.Fatal("first call should report wrote=true")
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("reading installed skill: %v", err)
	}
	if string(got) != Markdown() {
		t.Errorf("installed content does not match embed (%d vs %d bytes)", len(got), len(Markdown()))
	}

	// Second call: content already current → no write.
	wrote, err = EnsureInstalled(home)
	if err != nil {
		t.Fatalf("second EnsureInstalled: %v", err)
	}
	if wrote {
		t.Error("second call should be a no-op (wrote=false)")
	}

	// Pre-seeded differing copy → overwritten from the embed (the embed
	// is the source of truth).
	if err := os.WriteFile(dest, []byte("stale hand-edited skill\n"), 0o644); err != nil {
		t.Fatalf("seeding stale copy: %v", err)
	}
	wrote, err = EnsureInstalled(home)
	if err != nil {
		t.Fatalf("overwrite EnsureInstalled: %v", err)
	}
	if !wrote {
		t.Error("differing copy should be overwritten (wrote=true)")
	}
	got, _ = os.ReadFile(dest)
	if string(got) != Markdown() {
		t.Error("stale copy was not overwritten from the embed")
	}
}

func TestEnsureInstalledEmptyHome(t *testing.T) {
	wrote, err := EnsureInstalled("")
	if err != nil || wrote {
		t.Errorf("empty home should be a silent no-op, got wrote=%v err=%v", wrote, err)
	}
}

func TestEmbedNonEmpty(t *testing.T) {
	if len(Markdown()) == 0 {
		t.Fatal("embedded SKILL.md is empty")
	}
	if SkillName == "" {
		t.Fatal("SkillName is empty")
	}
}
