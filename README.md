# lightweight-prow-approve

Lightweight (GitHub / GitLab runner-based) version of the Prow "LGTM" and
"Approve" plugins

## Goal

The goal of this plugin s to enable prow-style `/lgtm` and `/approve` workflows
(as well as approving PR reviews) using
[GitHub Actions](https://docs.github.com/en/actions/) and
[GitHub Checks](https://docs.github.com/en/rest/checks) to block merging of PRs
until a sufficient number of qualified reviewers have commented on the PR.

## Why not just use `CODEOWNERS`?

Working on the Knative project, we attempted to migrate from Prow `OWNERS` files
to the `CODEOWNERS` feature. Unfortunately, we found several problems, some more
severe than others:

- `CODEOWNERS` is a top-level file, which requires top-level review to delegate
  permissions (minor difficulty)
- `CODEOWNERS` does not distinguish between PRs from maintainers and those from
  other contributors (major pain point)
  - In Knative, we want to keep the set of people who understand ("own") the
    code fairly small, without substantially getting in the way. We also want to
    ensure that each change has been looked at by at least two sets of eyes.
  - Unfortunately, `CODEOWNERS` enforces that the additional set of eyes
    **must** be a maintainer, even if the first set of eyes was from a
    maintainer. In practice, this means that non-maintainers can't do useful
    reviews, and increases the load on existing maintainers.
