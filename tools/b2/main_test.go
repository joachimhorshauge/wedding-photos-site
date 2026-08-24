package main

import (
	"path/filepath"
	"testing"
)

func TestSha1Matches(t *testing.T) {
	const sum = "af8b86febc0a0883f33999891c07e432221f0666"

	for _, tc := range []struct {
		name     string
		recorded string
		got      string
		want     bool
	}{
		{"plain sha1", sum, sum, true},
		{"uppercase from the API", "AF8B86FEBC0A0883F33999891C07E432221F0666", sum, true},
		// How anything uploaded through a presigned S3 PUT is stored.
		{"unverified prefix", "unverified:" + sum, sum, true},
		{"large file has no whole-file hash", "none", sum, true},
		{"empty is treated as absent", "", sum, true},
		{"a real mismatch still fails", sum, "0000000000000000000000000000000000000000", false},
		{"unverified mismatch still fails", "unverified:" + sum, "0000000000000000000000000000000000000000", false},
	} {
		if got := sha1Matches(tc.recorded, tc.got); got != tc.want {
			t.Errorf("%s: sha1Matches(%q, %q) = %v, want %v", tc.name, tc.recorded, tc.got, got, tc.want)
		}
	}
}

func TestPlan(t *testing.T) {
	const dir, prefix = "content", "photos/"

	for _, tc := range []struct {
		name         string
		key          string
		wantOK       bool
		wantDownload string
		wantGallery  string
		wantConvert  bool
	}{
		{
			name: "photo in the home gallery", key: "photos/a.jpg", wantOK: true,
			wantDownload: filepath.Join("content", "a.jpg"),
			wantGallery:  filepath.Join("content", "a.jpg"),
		},
		{
			name: "subdirectory becomes an album", key: "photos/dag-to/b.jpg", wantOK: true,
			wantDownload: filepath.Join("content", "dag-to", "b.jpg"),
			wantGallery:  filepath.Join("content", "dag-to", "b.jpg"),
		},
		{
			name: "HEIC is staged and converted", key: "photos/c.HEIC", wantOK: true,
			wantDownload: filepath.Join("content", stagingDir, "c.HEIC"),
			wantGallery:  filepath.Join("content", "c.jpg"),
			wantConvert:  true,
		},
		{
			// Cleaning against a leading slash absorbs the traversal rather
			// than refusing it, so the file still lands inside content/.
			name: "traversal cannot escape the content directory", key: "photos/../../evil.jpg", wantOK: true,
			wantDownload: filepath.Join("content", "evil.jpg"),
			wantGallery:  filepath.Join("content", "evil.jpg"),
		},
		{name: "the archive itself is not a photo", key: "album.zip"},
		{name: "non-image is skipped", key: "photos/notes.txt"},
		{name: "the prefix itself is not a file", key: "photos/"},
	} {
		at, ok := plan(dir, prefix, tc.key)
		if ok != tc.wantOK {
			t.Errorf("%s: plan(%q) ok = %v, want %v", tc.name, tc.key, ok, tc.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if at.download != tc.wantDownload || at.gallery != tc.wantGallery || at.convert != tc.wantConvert {
			t.Errorf("%s: plan(%q) = %+v, want download=%q gallery=%q convert=%v",
				tc.name, tc.key, at, tc.wantDownload, tc.wantGallery, tc.wantConvert)
		}
	}
}
