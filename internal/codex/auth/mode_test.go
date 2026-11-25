//go:build unix

package auth

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// permModes is the table the permission diagnostics are checked over: the
// private mode the Codex CLI writes, and each common way it gets widened.
var permModes = []struct {
	mode    os.FileMode
	exposed bool
}{
	{0o600, false},
	{0o640, true},
	{0o644, true},
	{0o660, true},
	{0o666, true},
	// Single bits, so each of group-read, group-write, other-read and
	// other-write is shown to count on its own, and execute-only does not.
	{0o620, true},
	{0o604, true},
	{0o602, true},
	{0o610, false},
	{0o700, false},
}

func freshAuthBody() []byte {
	return authJSON{
		access:    makeJWT(time.Now().Add(2 * time.Hour)),
		refresh:   "refresh-canary",
		accountID: "acct-mode",
	}.bytes()
}

func TestPeekReportsGroupOrOtherAccess(t *testing.T) {
	for _, tc := range permModes {
		t.Run(tc.mode.String(), func(t *testing.T) {
			f := newFakeOAuth(t)
			s, path := newTestSource(t, f, freshAuthBody())
			s.log = slogTextLogger(&bytes.Buffer{})
			if err := os.Chmod(path, tc.mode); err != nil {
				t.Fatal(err)
			}
			st := s.Peek()
			if st.State != StateOK {
				t.Fatalf("State = %q, want ok", st.State)
			}
			if st.FileMode != tc.mode {
				t.Errorf("FileMode = %v, want %v", st.FileMode, tc.mode)
			}
			if st.GroupOrOtherAccess != tc.exposed {
				t.Errorf("GroupOrOtherAccess = %t, want %t", st.GroupOrOtherAccess, tc.exposed)
			}
			if got := st.ModeWarning(); (got != "") != tc.exposed {
				t.Errorf("ModeWarning() = %q, want a warning exactly when exposed (%t)", got, tc.exposed)
			} else if tc.exposed && !strings.Contains(got, "permits access by group/others") {
				t.Errorf("ModeWarning() = %q, want the fixed group/others wording", got)
			}
		})
	}
}

// TestPeekSeesAChmodWithoutARefresh: the diagnostic comes from the stat Peek
// already does, so a chmod between two polls shows up on the next one without
// any Get, refresh or cache invalidation in between.
func TestPeekSeesAChmodWithoutARefresh(t *testing.T) {
	f := newFakeOAuth(t)
	s, path := newTestSource(t, f, freshAuthBody())
	s.log = slogTextLogger(&bytes.Buffer{})
	if _, err := s.Get(context.Background()); err != nil {
		t.Fatalf("Get: %v", err)
	}
	for _, step := range []struct {
		mode    os.FileMode
		exposed bool
	}{{0o600, false}, {0o644, true}, {0o640, true}, {0o600, false}} {
		if err := os.Chmod(path, step.mode); err != nil {
			t.Fatal(err)
		}
		st := s.Peek()
		if st.FileMode != step.mode || st.GroupOrOtherAccess != step.exposed {
			t.Errorf("after chmod %v: FileMode=%v GroupOrOtherAccess=%t, want %v/%t",
				step.mode, st.FileMode, st.GroupOrOtherAccess, step.mode, step.exposed)
		}
	}
	if n := f.calls.Load(); n != 0 {
		t.Errorf("mode tracking caused %d refreshes", n)
	}
}

// TestPeekReportsModeOfAnUnparseableFile: a file that exists but cannot be
// used is still a credential-bearing file whose mode is worth reporting.
func TestPeekReportsModeOfAnUnparseableFile(t *testing.T) {
	f := newFakeOAuth(t)
	s, path := newTestSource(t, f, []byte("{not json"))
	s.log = slogTextLogger(&bytes.Buffer{})
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	st := s.Peek()
	if st.State != StateMissing || st.FileMode != 0o644 || !st.GroupOrOtherAccess {
		t.Errorf("Peek = %+v, want missing with mode 0644 flagged", st)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if st := s.Peek(); st.FileMode != 0 || st.GroupOrOtherAccess {
		t.Errorf("Peek of an absent file = %+v, want no mode", st)
	}
}

// TestPermissiveModeIsLoggedOncePerChange: a health poll every few seconds
// must not turn one loose chmod into a warning per poll. The warning fires
// when the condition is first seen and again only when the mode changes.
func TestPermissiveModeIsLoggedOncePerChange(t *testing.T) {
	f := newFakeOAuth(t)
	s, path := newTestSource(t, f, freshAuthBody())
	var logs bytes.Buffer
	s.log = slogTextLogger(&logs)
	warnings := func() int { return strings.Count(logs.String(), "permits access by group/others") }

	chmodAndPoll := func(mode os.FileMode, polls int) {
		t.Helper()
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < polls; i++ {
			s.Peek()
		}
	}

	chmodAndPoll(0o600, 3)
	if n := warnings(); n != 0 {
		t.Fatalf("private mode produced %d warnings", n)
	}
	chmodAndPoll(0o644, 5)
	// The load path stats too; it must share the same once-per-change state.
	if _, err := s.Get(context.Background()); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if n := warnings(); n != 1 {
		t.Fatalf("0644 polled repeatedly: %d warnings, want 1", n)
	}
	if !strings.Contains(logs.String(), "0644") {
		t.Errorf("warning does not name the mode: %s", logs.String())
	}
	chmodAndPoll(0o640, 3)
	if n := warnings(); n != 2 {
		t.Fatalf("mode changed 0644 -> 0640: %d warnings, want 2", n)
	}
	chmodAndPoll(0o600, 3)
	if n := warnings(); n != 2 {
		t.Fatalf("mode tightened to 0600: %d warnings, want still 2", n)
	}
	chmodAndPoll(0o644, 3)
	if n := warnings(); n != 3 {
		t.Fatalf("mode loosened again: %d warnings, want 3", n)
	}
	if strings.Contains(logs.String(), "refresh-canary") {
		t.Error("a log line carried token material")
	}
}

// TestRefreshWritePreservesEachMode: utraque is a guest writer of a file the
// Codex CLI owns, so a refresh write-back keeps whatever mode the file had,
// loose or not; the diagnostic above is how a loose mode is surfaced.
func TestRefreshWritePreservesEachMode(t *testing.T) {
	for _, tc := range permModes {
		t.Run(tc.mode.String(), func(t *testing.T) {
			f := newFakeOAuth(t)
			s, path := newTestSource(t, f, authJSON{
				access:    makeJWT(time.Now().Add(30 * time.Second)), // within skew => stale
				refresh:   "refresh-canary",
				accountID: "acct-mode",
			}.bytes())
			s.log = slogTextLogger(&bytes.Buffer{})
			if err := os.Chmod(path, tc.mode); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Get(context.Background()); err != nil {
				t.Fatalf("Get: %v", err)
			}
			if n := f.calls.Load(); n != 1 {
				t.Fatalf("refreshes = %d, want 1 (the write path must run)", n)
			}
			fi, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode().Perm() != tc.mode {
				t.Errorf("mode after write-back = %v, want %v", fi.Mode().Perm(), tc.mode)
			}
		})
	}
}

func gidOf(t *testing.T, path string) int {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return int(fi.Sys().(*syscall.Stat_t).Gid)
}

// groupMismatchFixture writes an auth.json whose group differs from the group
// a new file in its directory gets (on BSD/macOS a new file takes the
// directory's group; on Linux the creating process's), which is exactly the
// situation in which a temp-file-and-rename can change the file's group. It
// returns the path, the original group and the new-file group, or skips when
// the user belongs to only one group.
func groupMismatchFixture(t *testing.T, mode os.FileMode) (path string, origGID, newFileGID int) {
	t.Helper()
	dir := t.TempDir()
	probe := filepath.Join(dir, "probe")
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	newFileGID = gidOf(t, probe)
	if err := os.Remove(probe); err != nil {
		t.Fatal(err)
	}
	groups, err := os.Getgroups()
	if err != nil {
		t.Skipf("cannot list groups: %v", err)
	}
	origGID = -1
	for _, g := range groups {
		if g != newFileGID {
			origGID = g
			break
		}
	}
	if origGID < 0 {
		t.Skip("the test user belongs to only one group")
	}
	path = filepath.Join(dir, "auth.json")
	if err := os.WriteFile(path, []byte(`{"tokens":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(path, -1, origGID); err != nil {
		t.Skipf("cannot chgrp the fixture: %v", err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path, origGID, newFileGID
}

// TestWriteKeepsTheFilesGroup: restoring the old mode bits onto a file that
// silently changed group would hand the group bits to a different group. The
// write must keep the original group.
func TestWriteKeepsTheFilesGroup(t *testing.T) {
	for _, mode := range []os.FileMode{0o640, 0o660, 0o600} {
		t.Run(mode.String(), func(t *testing.T) {
			path, origGID, _ := groupMismatchFixture(t, mode)
			if _, err := writeAuthFileAtomic(path, []byte(`{"tokens":{"x":"y"}}`), mode); err != nil {
				t.Fatalf("writeAuthFileAtomic: %v", err)
			}
			if got := gidOf(t, path); got != origGID {
				t.Errorf("group after write = %d, want the original %d", got, origGID)
			}
			fi, _ := os.Stat(path)
			if fi.Mode().Perm() != mode {
				t.Errorf("mode after write = %v, want %v", fi.Mode().Perm(), mode)
			}
		})
	}
}

// TestWriteNarrowsWhenTheOriginalVanished: if the file is deleted between the
// read and the rename (the Codex CLI logging out, say) there is no original
// group to keep, so the group and other bits are narrowed exactly as for a
// refused chgrp: 0640 and 0660 become 0600, 0644 survives.
func TestWriteNarrowsWhenTheOriginalVanished(t *testing.T) {
	for _, tc := range []struct{ in, want os.FileMode }{
		{0o640, 0o600},
		{0o660, 0o600},
		{0o644, 0o644},
		{0o600, 0o600},
	} {
		t.Run(tc.in.String(), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "auth.json")
			if err := os.WriteFile(path, []byte(`{"tokens":{}}`), tc.in); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, tc.in); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if _, err := writeAuthFileAtomic(path, []byte(`{"tokens":{"x":"y"}}`), tc.in); err != nil {
				t.Fatalf("writeAuthFileAtomic: %v", err)
			}
			fi, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode().Perm() != tc.want {
				t.Errorf("mode = %v, want %v", fi.Mode().Perm(), tc.want)
			}
		})
	}
	// The same decision, asked of keepGroup directly.
	tmp := filepath.Join(t.TempDir(), "tmp")
	if err := os.WriteFile(tmp, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := keepGroup(tmp, filepath.Join(t.TempDir(), "gone"), 0o640); got != 0o600 {
		t.Errorf("keepGroup with no original = %v, want 0600", got)
	}
}

// TestWriteNeverWidensWhenTheGroupCannotBeKept: if the original group cannot
// be restored, the new file's group and other bits are both narrowed to what
// the old group AND the old others were allowed, so nobody gains access: a
// member of the new group was an "other" before, and a member of the old
// group is an "other" now.
func TestWriteNeverWidensWhenTheGroupCannotBeKept(t *testing.T) {
	orig := chownFile
	t.Cleanup(func() { chownFile = orig })
	chownFile = func(string, int, int) error { return errors.New("chown refused (test)") }

	for _, tc := range []struct{ in, want os.FileMode }{
		{0o600, 0o600},
		{0o640, 0o600},
		{0o660, 0o600},
		{0o604, 0o600},
		{0o644, 0o644},
		{0o666, 0o666},
		{0o664, 0o644},
	} {
		t.Run(tc.in.String(), func(t *testing.T) {
			path, _, newFileGID := groupMismatchFixture(t, tc.in)
			if _, err := writeAuthFileAtomic(path, []byte(`{"tokens":{"x":"y"}}`), tc.in); err != nil {
				t.Fatalf("writeAuthFileAtomic: %v", err)
			}
			if got := gidOf(t, path); got != newFileGID {
				t.Fatalf("group = %d, want the new-file group %d (chown was refused)", got, newFileGID)
			}
			fi, _ := os.Stat(path)
			if fi.Mode().Perm() != tc.want {
				t.Errorf("mode = %v, want %v", fi.Mode().Perm(), tc.want)
			}
		})
	}
}
