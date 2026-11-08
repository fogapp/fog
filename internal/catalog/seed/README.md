# Catalog seed

The catalogs a release of fog ships with, one directory per catalog named
as in `DefaultRemotes` (`seed/fog-apps/`, `seed/fog-catalog/`). A fog reads
a catalog from here until its first fetch of that catalog succeeds, so a
machine with no route to the internet still has a store.

Nothing but this README is committed: `scripts/catalog-seed.sh` fills the
directory from checkouts of the catalog repositories, and
`scripts/release.sh` runs it before building.
