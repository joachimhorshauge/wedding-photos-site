// Command b2 syncs the photo album between a Backblaze B2 bucket and this Hugo site.
//
//	pull  downloads bucket objects into content/ as Hugo page resources,
//	      converting the odd iPhone HEIC into something Hugo can resize
//	zip   packs those photos into a single archive and uploads it to the bucket,
//	      so the site can offer a "download everything" link
//
// It speaks B2's native API, which authenticates with plain Basic auth, so no
// AWS SigV4 signing and no external dependencies are needed.
package main

import (
	"archive/zip"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const authURL = "https://api.backblazeb2.com/b2api/v3/b2_authorize_account"

// setHashKey names the file info entry that records which photos went into the
// archive, so an unchanged album is not re-uploaded on every build.
const setHashKey = "set-hash"

type client struct {
	http        *http.Client
	apiURL      string
	downloadURL string
	token       string
	bucketID    string
	bucketName  string
}

type fileInfo struct {
	FileName      string            `json:"fileName"`
	ContentLength int64             `json:"contentLength"`
	ContentSha1   string            `json:"contentSha1"`
	ContentType   string            `json:"contentType"`
	FileInfo      map[string]string `json:"fileInfo"`
}

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		usage()
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	bucket := fs.String("bucket", os.Getenv("B2_BUCKET"), "bucket name (or B2_BUCKET)")
	prefix := fs.String("prefix", envOr("B2_PREFIX", "photos/"), "key prefix holding the photos (or B2_PREFIX)")
	dir := fs.String("dir", "content", "local directory of Hugo page resources")

	var (
		prune    *bool
		workers  *int
		zipName  *string
		force    *bool
		manifest *string
	)
	switch cmd {
	case "pull":
		prune = fs.Bool("prune", true, "delete local photos that no longer exist in the bucket")
		workers = fs.Int("workers", 8, "parallel downloads")
	case "zip":
		zipName = fs.String("name", envOr("B2_ZIP_KEY", "album.zip"), "key to upload the archive to (or B2_ZIP_KEY)")
		force = fs.Bool("force", false, "upload even if the album is unchanged")
		manifest = fs.String("manifest", "data/album.json", "where to write the album summary Hugo reads")
	default:
		usage()
	}
	_ = fs.Parse(os.Args[2:])

	if *bucket == "" {
		log.Fatal("no bucket: pass -bucket or set B2_BUCKET")
	}
	keyID, appKey := os.Getenv("B2_KEY_ID"), os.Getenv("B2_APP_KEY")
	if keyID == "" || appKey == "" {
		log.Fatal("no credentials: set B2_KEY_ID and B2_APP_KEY")
	}

	c, err := authorize(keyID, appKey, *bucket)
	if err != nil {
		log.Fatalf("authorize: %v", err)
	}

	switch cmd {
	case "pull":
		err = pull(c, *prefix, *dir, *workers, *prune)
	case "zip":
		err = uploadZip(c, *prefix, *dir, *zipName, *manifest, *force)
	}
	if err != nil {
		log.Fatalf("%s: %v", cmd, err)
	}
}

func usage() {
	log.Fatal("usage:\n  b2 pull [-bucket b] [-prefix photos/] [-dir content] [-prune] [-workers 8]\n  b2 zip  [-bucket b] [-prefix photos/] [-dir content] [-name album.zip] [-force]")
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// authorize exchanges the application key for a session token. A key that is
// restricted to one bucket already names it in the response; a broader key
// needs a lookup.
func authorize(keyID, appKey, bucket string) (*client, error) {
	req, _ := http.NewRequest(http.MethodGet, authURL, nil)
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(keyID+":"+appKey)))

	hc := &http.Client{Timeout: 5 * time.Minute}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		return nil, err
	}

	var out struct {
		AuthorizationToken string `json:"authorizationToken"`
		APIInfo            struct {
			StorageAPI struct {
				APIURL      string `json:"apiUrl"`
				DownloadURL string `json:"downloadUrl"`
				BucketID    string `json:"bucketId"`
				BucketName  string `json:"bucketName"`
			} `json:"storageApi"`
		} `json:"apiInfo"`
		AccountID string `json:"accountId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	s := out.APIInfo.StorageAPI

	c := &client{
		http:        hc,
		apiURL:      s.APIURL,
		downloadURL: s.DownloadURL,
		token:       out.AuthorizationToken,
		bucketID:    s.BucketID,
		bucketName:  bucket,
	}
	if s.BucketName != "" && s.BucketName != bucket {
		return nil, fmt.Errorf("key is restricted to bucket %q, not %q", s.BucketName, bucket)
	}
	if c.bucketID == "" {
		id, err := c.lookupBucket(out.AccountID, bucket)
		if err != nil {
			return nil, err
		}
		c.bucketID = id
	}
	return c, nil
}

func (c *client) lookupBucket(accountID, bucket string) (string, error) {
	var out struct {
		Buckets []struct {
			BucketID   string `json:"bucketId"`
			BucketName string `json:"bucketName"`
		} `json:"buckets"`
	}
	body := map[string]string{"accountId": accountID, "bucketName": bucket}
	if err := c.call("b2_list_buckets", body, &out); err != nil {
		return "", err
	}
	for _, b := range out.Buckets {
		if b.BucketName == bucket {
			return b.BucketID, nil
		}
	}
	return "", fmt.Errorf("bucket %q not found", bucket)
}

func (c *client) call(op string, body, out any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.apiURL+"/b2api/v3/"+op, strings.NewReader(string(buf)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func checkStatus(resp *http.Response) error {
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
}

// list returns every object under prefix, following B2's pagination.
func (c *client) list(prefix string) ([]fileInfo, error) {
	var all []fileInfo
	start := ""
	for {
		var out struct {
			Files        []fileInfo `json:"files"`
			NextFileName *string    `json:"nextFileName"`
		}
		body := map[string]any{"bucketId": c.bucketID, "prefix": prefix, "maxFileCount": 1000}
		if start != "" {
			body["startFileName"] = start
		}
		if err := c.call("b2_list_file_names", body, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Files...)
		if out.NextFileName == nil || *out.NextFileName == "" {
			return all, nil
		}
		start = *out.NextFileName
	}
}

var imageExt = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".gif": true}

// heicExt lists what phones upload that Hugo's image processing cannot read.
var heicExt = map[string]bool{".heic": true, ".heif": true}

// stagingDir holds files that are not page resources themselves, only the
// source of one. Hugo ignores directories whose name starts with a dot.
const stagingDir = ".originals"

// placement says where a bucket object lands locally: download is where the
// bytes are written, gallery is the page resource Hugo ends up seeing. They
// differ only for formats that have to be converted first.
type placement struct {
	download string
	gallery  string
	convert  bool
}

// plan maps a bucket key to its place in the Hugo content tree. Objects
// directly under the prefix belong to the home page's gallery; a subdirectory
// becomes its own album.
func plan(dir, prefix, key string) (placement, bool) {
	rel := strings.TrimPrefix(key, prefix)
	if rel == "" || strings.HasSuffix(rel, "/") {
		return placement{}, false
	}
	// Reject anything that would escape the content directory.
	clean := strings.TrimPrefix(path.Clean("/"+rel), "/")
	if strings.Contains(clean, "..") {
		return placement{}, false
	}
	ext := strings.ToLower(path.Ext(clean))
	local := filepath.Join(dir, filepath.FromSlash(clean))

	switch {
	case imageExt[ext]:
		return placement{download: local, gallery: local}, true
	case heicExt[ext]:
		staged := filepath.Join(dir, stagingDir, filepath.FromSlash(clean))
		return placement{
			download: staged,
			gallery:  strings.TrimSuffix(local, filepath.Ext(local)) + ".jpg",
			convert:  true,
		}, true
	}
	return placement{}, false
}

func pull(c *client, prefix, dir string, workers int, prune bool) error {
	files, err := c.list(prefix)
	if err != nil {
		return err
	}

	type job struct {
		file fileInfo
		at   placement
	}
	var jobs []job
	keep := map[string]bool{}
	albums := map[string]bool{}
	// Which bucket key backs each photo, for the download buttons.
	origins := map[string]string{}

	for _, f := range files {
		at, ok := plan(dir, prefix, f.FileName)
		if !ok {
			continue
		}
		keep[at.download] = true
		keep[at.gallery] = true
		if sub := filepath.Dir(at.gallery); sub != filepath.Clean(dir) {
			albums[sub] = true
		}
		// A converted photo has no full-size counterpart in the bucket worth
		// linking to: it is in a format half the guests cannot open.
		if !at.convert {
			origins[filepath.ToSlash(mustRel(dir, at.gallery))] = f.FileName
		}
		have := false
		if st, err := os.Stat(at.download); err == nil && st.Size() == f.ContentLength {
			have = true
		}
		if have && !at.convert {
			continue // already have it; B2 keys are unique per upload
		}
		if have && at.convert {
			if _, err := os.Stat(at.gallery); err == nil {
				continue
			}
		}
		jobs = append(jobs, job{f, at})
	}

	log.Printf("bucket has %d photos, %d to download", len(keep), len(jobs))

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		done     int
	)
	ch := make(chan job)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range ch {
				err := c.fetch(j.file, j.at)
				mu.Lock()
				if err != nil && firstErr == nil {
					firstErr = err
				}
				done++
				if done%25 == 0 {
					log.Printf("  %d/%d", done, len(jobs))
				}
				mu.Unlock()
			}
		}()
	}
	for _, j := range jobs {
		ch <- j
	}
	close(ch)
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}

	for sub := range albums {
		if err := ensureAlbumIndex(sub); err != nil {
			return err
		}
	}
	if err := writeOrigins(filepath.Join("data", "originals.json"), origins); err != nil {
		return err
	}
	if prune {
		if err := pruneExtras(dir, keep); err != nil {
			return err
		}
	}
	return nil
}

// fetch downloads one object and, when the format needs it, converts the
// result into a JPEG that Hugo can resize.
func (c *client) fetch(f fileInfo, at placement) error {
	if st, err := os.Stat(at.download); err != nil || st.Size() != f.ContentLength {
		if err := c.download(f, at.download); err != nil {
			return err
		}
	}
	if !at.convert {
		return nil
	}
	return toJPEG(at.download, at.gallery)
}

// converters are tried in order; the first one present on the machine wins.
var converters = [][]string{
	{"heif-convert", "-q", "90", "{in}", "{out}"},
	{"magick", "{in}", "-quality", "90", "{out}"},
	{"convert", "{in}", "-quality", "90", "{out}"},
	{"ffmpeg", "-v", "error", "-y", "-i", "{in}", "-frames:v", "1", "-q:v", "2", "{out}"},
}

var (
	converterOnce sync.Once
	converterArgs []string
)

func toJPEG(src, dest string) error {
	converterOnce.Do(func() {
		for _, cand := range converters {
			if _, err := exec.LookPath(cand[0]); err == nil {
				converterArgs = cand
				return
			}
		}
	})
	if converterArgs == nil {
		log.Printf("warning: no HEIC converter found (install libheif-examples); skipping %s", filepath.Base(src))
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	args := make([]string, 0, len(converterArgs)-1)
	for _, a := range converterArgs[1:] {
		a = strings.ReplaceAll(a, "{in}", src)
		args = append(args, strings.ReplaceAll(a, "{out}", dest))
	}
	out, err := exec.Command(converterArgs[0], args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("convert %s: %v: %s", filepath.Base(src), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func mustRel(base, p string) string {
	rel, err := filepath.Rel(base, p)
	if err != nil {
		return filepath.Base(p)
	}
	return rel
}

// writeOrigins records, for each photo Hugo will see, the bucket key holding
// its full-size version. The templates use it to link the download buttons,
// and fall back to the published copy for photos that have no entry.
func writeOrigins(path string, origins map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(origins, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(body, '\n'), 0o644)
}

func (c *client) download(f fileInfo, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	u := c.downloadURL + "/file/" + c.bucketName + "/" + urlEncodePath(f.FileName)
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		return fmt.Errorf("download %s: %w", f.FileName, err)
	}

	// Write to a temporary file so an interrupted build never leaves a
	// truncated photo that the size check would later accept as complete.
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".b2-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	sum := sha1.New()
	if _, err := io.Copy(io.MultiWriter(tmp, sum), resp.Body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if !sha1Matches(f.ContentSha1, hex.EncodeToString(sum.Sum(nil))) {
		return fmt.Errorf("%s: checksum mismatch", f.FileName)
	}
	return os.Rename(tmp.Name(), dest)
}

// sha1Matches compares a download against the checksum B2 recorded for it.
//
// Two of B2's answers are not checksums at all. "none" comes back for large
// files, which store their hash per part. The "unverified:" prefix means the
// client never declared a hash when uploading, so B2 computed one itself and
// flags that it was not cross-checked - which is how everything arriving
// through a presigned S3 PUT is stored, and that is most of this bucket. Both
// still identify the bytes; only the promise about who computed them differs.
func sha1Matches(recorded, got string) bool {
	if recorded == "none" || recorded == "" {
		return true
	}
	return strings.EqualFold(strings.TrimPrefix(recorded, "unverified:"), got)
}

// urlEncodePath escapes each path segment but keeps the separators intact.
func urlEncodePath(key string) string {
	parts := strings.Split(key, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// ensureAlbumIndex gives a subdirectory the index.md that turns it into a Hugo
// leaf bundle, which is what makes the theme render it as an album.
func ensureAlbumIndex(dir string) error {
	p := filepath.Join(dir, "index.md")
	if _, err := os.Stat(p); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	title := strings.ReplaceAll(filepath.Base(dir), "-", " ")
	front := fmt.Sprintf("---\ntitle: %q\nbuild:\n  publishResources: false\n---\n", titleCase(title))
	return os.WriteFile(p, []byte(front), 0o644)
}

func titleCase(s string) string {
	words := strings.Fields(s)
	for i, w := range words {
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}

// pruneExtras removes photos that have disappeared from the bucket, so a
// deleted photo also leaves the published site.
func pruneExtras(dir string, keep map[string]bool) error {
	removed := 0
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		ext := strings.ToLower(filepath.Ext(p))
		if !imageExt[ext] && !heicExt[ext] {
			return nil
		}
		if keep[p] {
			return nil
		}
		removed++
		return os.Remove(p)
	})
	if removed > 0 {
		log.Printf("pruned %d photo(s) no longer in the bucket", removed)
	}
	return err
}

func uploadZip(c *client, prefix, dir, key, manifest string, force bool) error {
	files, err := c.list(prefix)
	if err != nil {
		return err
	}
	var members []string
	h := sha256.New()
	for _, f := range files {
		at, ok := plan(dir, prefix, f.FileName)
		if !ok {
			continue
		}
		if _, err := os.Stat(at.gallery); err != nil {
			continue // never converted; nothing to put in the archive
		}
		members = append(members, at.gallery)
		fmt.Fprintf(h, "%s\t%d\t%s\n", f.FileName, f.ContentLength, f.ContentSha1)
	}
	sort.Strings(members)
	want := hex.EncodeToString(h.Sum(nil))

	existing, err := c.list(key)
	if err != nil {
		return err
	}
	for _, e := range existing {
		if e.FileName == key && e.FileInfo[setHashKey] == want && !force {
			log.Printf("%s is already current (%d photos)", key, len(members))
			return writeManifest(manifest, len(members), e.ContentLength)
		}
	}

	tmp, err := os.CreateTemp("", "album-*.zip")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	// Store rather than deflate: JPEGs are already compressed, so deflating
	// them burns CI time for roughly nothing.
	zw := zip.NewWriter(tmp)
	for _, m := range members {
		if err := addToZip(zw, m); err != nil {
			return err
		}
	}
	if err := zw.Close(); err != nil {
		return err
	}

	size, sum, err := hashFile(tmp)
	if err != nil {
		return err
	}
	log.Printf("uploading %s (%d photos, %.0f MB)", key, len(members), float64(size)/1e6)
	if err := c.upload(tmp, key, size, sum, map[string]string{setHashKey: want}); err != nil {
		return err
	}
	return writeManifest(manifest, len(members), size)
}

// writeManifest records what the archive holds so the download button can say
// how much it is about to hand over.
func writeManifest(path string, count int, size int64) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(map[string]any{
		"count":     count,
		"bytes":     size,
		"sizeLabel": sizeLabel(size),
		"updated":   time.Now().UTC().Format(time.RFC3339),
	}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(body, '\n'), 0o644)
}

// sizeLabel counts in decimal megabytes, the way Backblaze's console and most
// people do, with a Danish decimal comma.
func sizeLabel(size int64) string {
	if size >= 1e9 {
		return strings.Replace(fmt.Sprintf("%.1f GB", float64(size)/1e9), ".", ",", 1)
	}
	return fmt.Sprintf("%.0f MB", float64(size)/1e6)
}

func addToZip(zw *zip.Writer, name string) error {
	f, err := os.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	hdr, err := zip.FileInfoHeader(st)
	if err != nil {
		return err
	}
	hdr.Name = filepath.Base(name)
	hdr.Method = zip.Store
	w, err := zw.CreateHeader(hdr)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, f)
	return err
}

func hashFile(f *os.File) (int64, string, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0, "", err
	}
	sum := sha1.New()
	n, err := io.Copy(sum, f)
	if err != nil {
		return 0, "", err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(sum.Sum(nil)), nil
}

func (c *client) upload(body io.Reader, key string, size int64, sha string, info map[string]string) error {
	var slot struct {
		UploadURL          string `json:"uploadUrl"`
		AuthorizationToken string `json:"authorizationToken"`
	}
	if err := c.call("b2_get_upload_url", map[string]string{"bucketId": c.bucketID}, &slot); err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, slot.UploadURL, body)
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("Authorization", slot.AuthorizationToken)
	req.Header.Set("X-Bz-File-Name", urlEncodePath(key))
	req.Header.Set("Content-Type", "application/zip")
	req.Header.Set("X-Bz-Content-Sha1", sha)
	for k, v := range info {
		req.Header.Set("X-Bz-Info-"+k, url.QueryEscape(v))
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return checkStatus(resp)
}
