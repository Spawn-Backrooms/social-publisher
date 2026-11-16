# Contributing

Thanks for helping. Good places to start: provider adapters (`internal/provider`), scheduling and retries (`internal/worker`), the API (`internal/api`), docs, and tests that cover edge cases.

## Sign off your commits (DCO)

Every commit needs a `Signed-off-by` line certifying the [Developer Certificate of Origin](https://developercertificate.org/): you wrote the change, or otherwise have the right to submit it under Apache-2.0.

```sh
git commit -s -m "Describe the change"
```

Pull requests with commits that aren't signed off fail the `dco` check. To fix a branch: `git rebase --signoff main`, then force-push it.

## Checks

```sh
make test
make test-race
make vet
```

PostgreSQL integration tests skip without `TEST_DATABASE_URL`. To run them, see [database testing](docs/deployment.md#database-testing). CI runs everything, including the native PostgreSQL gate, on every pull request with a read-only token and no secrets. First-time contributors' runs wait for maintainer approval.

## Ground rules

- Never commit credentials, provider app secrets, access tokens or real account data. Secret scanning and push protection are on.
- Don't make live provider calls in tests. Use the local HTTP fixtures.
- Keep honest status: don't describe a provider flow as verified unless you ran it, and say how.

## How changes reach SPAWN

SPAWN runs this service from a private repository that stays the source of truth. A maintainer ports merged pull requests there, keeping your authorship and sign-off, and the next export updates this repository.
