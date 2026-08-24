# Bryllupsbilleder

The finished album from the wedding: every photo on one page, in the order they
were taken, with a lightbox and download buttons. It is the read side of
[photo-share-wedding](../photo-share-wedding), which is what the guests uploaded
through on the day — same Backblaze bucket, opposite direction.

```
Backblaze bucket ──list+download──▶ GitHub Actions ──▶ Hugo ──▶ GitHub Pages
  photos/*.jpg      (build time)          │              thumbnails +
  album.zip ◀────── zip + upload ─────────┘              1600px lightbox copies
      ▲
      └── browser downloads full-size originals straight from the bucket
```

Photos are never committed. Each build mirrors the bucket into `content/` and
Hugo derives a 600px thumbnail from every photo. Only those thumbnails are
deployed — around 24 MB — because the lightbox, the tile links and both download
buttons all point at the originals in the bucket, which are already public.
Publishing a second full-size copy to Pages would just be the same bytes twice.

Guests can still be uploading: the site picks up whatever is in the bucket at
build time, and the workflow rebuilds daily.

## How photos get onto the site

`tools/b2 pull` lists the bucket and downloads what is missing. It speaks B2's
native API, which authenticates with plain Basic auth, so the build needs no AWS
signing and no dependencies beyond the Go standard library.

Three details are worth knowing:

- **Ordering: by upload, not by capture.** The uploader names files by upload
  time in UTC (`2026-08-22T15-03-08-364-a5d53c11.jpg`), so sorting by filename
  is chronological in the order guests *shared* photos, and the caption under
  each photo is that timestamp shifted to local time (`params.photoUTCOffset`).

  Sorting by when a photo was actually taken is not possible, and it is worth
  writing down why so nobody goes looking again: the uploader resizes each
  photo with `canvas.toBlob()`, and a canvas re-encode drops EXIF. Across all
  381 photos the only tags that survive are `ColorSpace`, `ExifImageWidth` and
  `ExifImageHeight` - no `DateTimeOriginal`, no camera make or model. The B2
  objects carry no custom metadata either. The capture time is simply not
  recorded anywhere.

  The practical effect is that a guest who uploads two days late lands at the
  end of the album rather than beside the moment they photographed. Fixing it
  means changing the uploader - either preserving the EXIF block through the
  resize, or sending `File.lastModified` - and would only help photos uploaded
  after that change.
- **HEIC.** Four photos came off iPhones as HEIC, which Hugo cannot resize -
  and which browsers other than Safari cannot display either, so the slideshow
  on the upload site could not show them. They have been converted and replaced
  in the bucket; the originals sit in `.heic-backup/` until the JPEGs are
  trusted. The conversion path stays in the sync tool for the next HEIC that
  turns up: such a file is staged under `content/.originals/` and converted with
  whichever of `heif-convert`, ImageMagick or `ffmpeg` is installed. Without a
  converter the build still succeeds and says which photos it skipped.
- **Deletions.** A photo removed from the bucket is removed from `content/` on
  the next pull, so it also leaves the site.

`tools/b2 zip` packs the same photos into `album.zip`, uploads it next to them,
and writes `data/album.json` so the button can say how large the download is.
The archive is tagged with a hash of the photo set, so an unchanged album is not
re-uploaded on every build.

## Downloads

**Download alle billeder** links straight to `album.zip`. Backblaze serves it as
`application/zip`, so browsers save it rather than open it.

**The button in the lightbox** saves the full-size original from the bucket.
Browsers ignore a `download` attribute on a cross-origin link, so the file is
fetched as a blob first, which the bucket has to allow with a CORS rule. The
bucket already has one — `weddingShareFromPagesOrigin`, added for the upload
site — and it covers this: `s3_get` and `s3_head` from
`https://joachimhorshauge.github.io`, which is the origin Pages serves from.

It does not cover `http://localhost:1313`, so downloads fall back during local
development. That is by design rather than a bug to route around: the site
checks CORS once on load and, when the fetch is not allowed, leaves the button
as an ordinary link that opens the photo in a new tab. Add localhost to the
rule's origins if you want the real thing while developing.

## Setup

1. **A Backblaze application key restricted to the bucket**, with `listFiles`,
   `readFiles` and `writeFiles`. Write access is only used to upload
   `album.zip`; the photos themselves are never touched.
2. **Repository secrets** `B2_KEY_ID` and `B2_APP_KEY` (Settings → Secrets and
   variables → Actions).
3. **Pages source: GitHub Actions** (Settings → Pages).

The bucket name, prefix and archive key live in `.github/workflows/deploy.yml`;
the public bucket URL the browser uses lives in `hugo.toml` under `params.b2`.

## Local development

```sh
just pull   # mirror the bucket into content/ (needs B2_KEY_ID and B2_APP_KEY in .env)
just dev    # http://localhost:1313/wedding-photos-site/
```

A cold build resizes 668 images and takes about a minute; after that Hugo reuses
`resources/` and rebuilds in a second or two. CI caches both `content/` and
`resources/` under a key naming the exact photo set, so a build that finds
nothing new in the bucket does almost no work.

## Theme

[hugo-theme-gallery](https://github.com/nicokaiser/hugo-theme-gallery) v4.9.3,
pinned as a submodule. Two of its files are forked, each with a header saying
what changed and why:

- `layouts/partials/gallery.html` — tiles carry the bucket key of their
  full-size original, and get a date caption
- `assets/js/lightbox.js` — the download button saves that original instead of
  the published 1600px copy

Diff them against the theme when bumping the submodule.
