# Local development. Pulling photos and publishing the archive need Backblaze
# credentials; put them in .env (git-ignored):
#
#   B2_KEY_ID=005...
#   B2_APP_KEY=K005...
#
set dotenv-load := true

bucket := env_var_or_default("B2_BUCKET", "wedding-photos-joachim-sarah")

# Serve the site at http://localhost:1313 with whatever photos are in content/.
dev:
    hugo server --disableFastRender

# Mirror the bucket into content/. Only downloads photos that are missing.
pull:
    go run ./tools/b2 pull -bucket {{bucket}}

# Rebuild album.zip and upload it, if the set of photos changed.
zip:
    go run ./tools/b2 zip -bucket {{bucket}}

# What CI does, minus the deploy.
build: pull zip
    hugo --minify

check:
    gofmt -l tools
    go vet ./tools/...
