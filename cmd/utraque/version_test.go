package main

import (
	"runtime/debug"
	"testing"
)

func TestBuildVersionStampedWins(t *testing.T) {
	info := &debug.BuildInfo{Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "0123456789abcdef"},
		{Key: "vcs.modified", Value: "true"},
	}}
	got := buildVersion("9.9.9-custom", info, true)
	if want := "9.9.9-custom"; got != want {
		t.Errorf("buildVersion = %q, want %q", got, want)
	}
}

func TestBuildVersionCleanRevision(t *testing.T) {
	info := &debug.BuildInfo{Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "0ef929d5f9b4abcdef"},
		{Key: "vcs.modified", Value: "false"},
	}}
	got := buildVersion("", info, true)
	if want := releaseVersion + "+0ef929d"; got != want {
		t.Errorf("buildVersion = %q, want %q", got, want)
	}
}

func TestBuildVersionDirtyRevision(t *testing.T) {
	info := &debug.BuildInfo{Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "0ef929d5f9b4abcdef"},
		{Key: "vcs.modified", Value: "true"},
	}}
	got := buildVersion("", info, true)
	if want := releaseVersion + "+0ef929d.dirty"; got != want {
		t.Errorf("buildVersion = %q, want %q", got, want)
	}
}

func TestBuildVersionShortRevisionNotTruncated(t *testing.T) {
	info := &debug.BuildInfo{Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "abc12"},
	}}
	got := buildVersion("", info, true)
	if want := releaseVersion + "+abc12"; got != want {
		t.Errorf("buildVersion = %q, want %q", got, want)
	}
}

func TestBuildVersionNoBuildInfo(t *testing.T) {
	if got := buildVersion("", nil, false); got != releaseVersion {
		t.Errorf("buildVersion = %q, want %q", got, releaseVersion)
	}
	// ok=false with a non-nil info (should not happen in practice, but the
	// function's contract is "trust ok", not "trust info != nil").
	if got := buildVersion("", &debug.BuildInfo{}, false); got != releaseVersion {
		t.Errorf("buildVersion = %q, want %q", got, releaseVersion)
	}
}

func TestBuildVersionNoRevisionSetting(t *testing.T) {
	info := &debug.BuildInfo{Settings: []debug.BuildSetting{
		{Key: "GOARCH", Value: "arm64"},
	}}
	if got := buildVersion("", info, true); got != releaseVersion {
		t.Errorf("buildVersion = %q, want %q", got, releaseVersion)
	}
}

func TestVersionRequested(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"no args", nil, false},
		{"unrelated flag", []string{"--listen", ":8080"}, false},
		{"double dash", []string{"--version"}, true},
		{"single dash", []string{"-version"}, true},
		{"among other args", []string{"--listen", ":8080", "-version"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := versionRequested(tc.args); got != tc.want {
				t.Errorf("versionRequested(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}
