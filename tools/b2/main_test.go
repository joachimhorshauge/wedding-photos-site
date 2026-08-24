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

func TestSplitParts(t *testing.T) {
	mb := func(n int64) int64 { return n * 1e6 }
	members := func(sizes ...int64) []member {
		var out []member
		for i, s := range sizes {
			out = append(out, member{path: string(rune('a'+i)) + ".jpg", size: s})
		}
		return out
	}

	t.Run("everything under the limit is one part", func(t *testing.T) {
		parts := splitParts(members(mb(100), mb(100), mb(100)), mb(400), "album")
		if len(parts) != 1 || parts[0].Key != "album-1.zip" || parts[0].Count != 3 {
			t.Fatalf("got %d parts, first %+v", len(parts), parts[0])
		}
	})

	t.Run("splits before crossing the limit", func(t *testing.T) {
		// 547 MB, the album's size when Cloudflare's 512 MB cache ceiling
		// forced the split, against the 400 MB default.
		parts := splitParts(members(mb(300), mb(150), mb(97)), mb(400), "album")
		if len(parts) != 2 {
			t.Fatalf("got %d parts, want 2", len(parts))
		}
		for _, p := range parts {
			if p.Bytes > mb(400) {
				t.Errorf("%s is %d bytes, over the limit", p.Key, p.Bytes)
			}
		}
		if parts[0].Count != 1 || parts[1].Count != 2 {
			t.Errorf("split in the wrong place: %d then %d", parts[0].Count, parts[1].Count)
		}
		if parts[1].Key != "album-2.zip" {
			t.Errorf("second part is named %q", parts[1].Key)
		}
	})

	t.Run("parts come out roughly even", func(t *testing.T) {
		// 400 photos of 1.1 MB against a 250 MB limit: two parts of ~220 MB,
		// not one of 250 MB and one of 190 MB.
		var sizes []int64
		for i := 0; i < 400; i++ {
			sizes = append(sizes, 1_100_000)
		}
		parts := splitParts(members(sizes...), mb(250), "album")
		if len(parts) != 2 {
			t.Fatalf("got %d parts, want 2", len(parts))
		}
		diff := parts[0].Bytes - parts[1].Bytes
		if diff < 0 {
			diff = -diff
		}
		if diff > mb(5) {
			t.Errorf("parts differ by %d bytes: %d vs %d", diff, parts[0].Bytes, parts[1].Bytes)
		}
		if parts[0].Count+parts[1].Count != 400 {
			t.Errorf("lost photos: %d + %d", parts[0].Count, parts[1].Count)
		}
	})

	t.Run("a photo bigger than the limit still gets archived", func(t *testing.T) {
		parts := splitParts(members(mb(500)), mb(400), "album")
		if len(parts) != 1 || parts[0].Count != 1 {
			t.Fatalf("oversized photo was dropped: %+v", parts)
		}
	})

	t.Run("no photos means no parts", func(t *testing.T) {
		if parts := splitParts(nil, mb(400), "album"); len(parts) != 0 {
			t.Fatalf("got %d parts from nothing", len(parts))
		}
	})
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
