---
name: release
description: Publish the current fog release to the public GitHub repo (fogapp/fog) as a single commit, without the private git history, plus the matching GitHub release of its signed files. Run after /ship.
disable-model-invocation: true
---

# Release: publish to public GitHub

`/ship` cuts a release on gitea.lan. This publishes it to
`github.com/fogapp/fog` in two parts:

1. the release's tree as one `Sync to vX.Y.Z` commit — private commits,
   subjects and tags stay private;
2. the GitHub release `vX.Y.Z`, tagged at that commit, carrying the signed
   files `/ship` left in `bin/release/` (binaries, `latest.json` and its
   `.sig`, `install.sh`, `flash.sh`, `fogd.service`, `fogd-rollback`,
   `SHA256SUMS`). This is what strangers install from and what their
   servers update from: `https://github.com/fogapp/fog` is the public
   default source, and it reads `releases/latest/download/latest.json`.

All of the mechanics live in `scripts/publish-public.sh` (which calls
`scripts/publish-github-release` for part 2). Never run the `commit-tree`,
`git push` or `gh release` by hand: a raw push of `main` uploads the entire
history, and that can't be taken back.

## 0. Preconditions

- `/ship` ran for this version **on this machine**, so `bin/release/` holds
  its signed files. The script checks the version, the signature against the
  pinned key, that the manifest is the relative one, and `SHA256SUMS`; if any
  check fails, rebuild with `./scripts/release.sh vX.Y.Z` (what `/ship`
  runs) — never edit `bin/release/` by hand.
- This clone names the public identity the sync commit is made under —
  never the owner's personal git identity:

  ```
  git config --get fog.publicAuthorName
  git config --get fog.publicAuthorEmail
  ```

  If either is empty, **stop** and ask the user to choose and set them
  (`git config fog.publicAuthorName '<name>'`, `git config
  fog.publicAuthorEmail '<email>'`). Don't pick values yourself. The
  script refuses without them.
- `gh` is logged in to github.com **as `fogapp`**. Check:

  ```
  gh auth status --hostname github.com
  ```

  If it isn't logged in, or shows another active account, **stop** and give
  the user these steps to run themselves (don't attempt the login — it
  needs a browser signed in as fogapp):

  ```
  gh auth login --hostname github.com --git-protocol ssh --web
  ```

  sign in as **fogapp** in the browser it opens, and skip the offer to
  upload an SSH key (fogapp's key, `~/.ssh/fog_ed25519`, is already on
  GitHub). If gh already knows fogapp as a second account:
  `gh auth switch --hostname github.com --user fogapp`. The script makes
  the same check and prints the same instructions.

## 1. Dry run

```
./scripts/publish-public.sh
```

This changes nothing on GitHub. It also runs the GitHub release check
(`bin/release/` and `gh`). If it stops, report why and don't work around it:

- **Not released:** run `/ship` first, or ask the user.
- **Dirty tree or main differs from origin:** tell the user; don't commit or
  push to fix it unasked.
- **`public/main` has commits this script didn't make:** someone changed the
  public repo. They need bringing into `main` first, or the sync erases them.
- **Secret scan hit:** go to step 2 before anything else. The scan covers
  binary files too, and treats a rename or copy to a name like `.env` as a
  new file.
- **"binary files over … that aren't web assets":** a large binary (a stray
  build at the root, say) is in the commit. Don't publish it and don't
  delete it: tell the user which file, so its owner can take it out of the
  tree.
- **"publish-github-release: …":** the release directory or `gh` isn't
  ready — see step 0. Report the script's message as it is.
- **"public main is already at vX.Y.Z":** the commit is out. `--push` then
  only makes sure the GitHub release is complete (useful after a failed
  upload); say so.

## 2. Read what's going public

The scan only recognises common credential formats, so read the change too:

```
git diff --stat public/main HEAD
git diff public/main HEAD -- <anything that looks sensitive>
```

Look for credentials the scan can't recognise, personal data, and new
directories or files that were never meant to be public. Existing `gitea.lan`
and `192.168.*` references are known and already public; a new one that
reveals something more isn't.

If anything is doubtful, stop and show the user the path and why, without
repeating a secret's value. Pass `--accept-findings` only once the user, or
your reading of the match, shows it's a false positive such as a test fixture,
and say which you accepted.

## 3. Publish

```
./scripts/publish-public.sh --push
```

Invoking `/release` is the user's go-ahead for this step, as long as step 2
was clean. It pushes the sync commit, then creates the GitHub release at that
commit — or, if the release already exists, replaces its assets in place
(`--clobber`), never a second release. If the commit went out but the release
step failed, fix the cause and run the same command again: it skips the push
and finishes the release.

## 4. Report

Give the version, the commit SHA the script printed, the file count from the
diff, the GitHub release URL (`https://github.com/fogapp/fog/releases/tag/vX.Y.Z`)
with its asset count, and any accepted findings. Say it's published only if
the script printed both `published github release fogapp/fog vX.Y.Z` and
`published vX.Y.Z`. If a secret reached GitHub, tell the user to rotate it: a
force-push doesn't undo the exposure.
