# Notes for anyone working on stripepay

Read [`README.md`](README.md) first: it has the reason the package exists, the verdicts, and
the design decisions with their motives.

## Language

**Everything in English**: identifiers, comments, documentation, error messages, test names.
It is a public Go module and it is read by people who have never seen the projects that use
it.

Test names are sentences saying what must happen, not which function they touch
(`TestForeignIsNotAnIncident`, not `TestCheckForeign`).

## The hard rules

1. **No global state.** Not the key, not the mode, nothing. One process must be able to
   charge into two accounts at once, because multi-tenant callers do exactly that.
2. **No API version pinned** on webhook endpoints. If a new field is ever needed, it is
   added to the type and that is all.
3. **No environment variables.** The key, the webhook secret and the service name all come
   in through `Options`. Reading them from a file is the application's job.
4. **The reference (`client_reference_id`) is required** when creating a session, and it is
   an unguessable id of your own. It is the only thing that ties a payment to what has to be
   delivered; a third party's reference goes in `Metadata`.
5. **Deferred payments are never accepted**, and it is enforced at both ends (on create and
   on verify).
6. **`ErrForeign` is not an error anybody is alerted about.** Any refactor of the error
   handling that puts it in the same bucket as `ErrAmount` reopens the false-alarm hole.
7. **No business logic here**: no VAT, no invoices, no deciding what gets delivered. See the
   last section of the README.

## The test vectors are the contract

`testdata/sessions.json` holds sessions and the verdict each one must produce. They are not
edited to make a test pass: if a verdict changes, that is a change of behaviour and it has
to be announced. A change here reaches every application that depends on the library at
once, so each of them should keep its own end-to-end test as well.

## Adding a field to `Session`

Cheap, and often the right move: add it to the type with its `json` tag, and if it is used
to decide whether to deliver, add a vector to `testdata/sessions.json` with its verdict.
Reading fields nobody uses buys nothing.

## Releasing

Versions are git tags:

```bash
go test ./... && go vet ./...
git tag -a v1.1.0 -m "…" && git push --tags
```

A behaviour change needs a new minor version at least, said plainly in the tag message, and
the applications that depend on it have to be told.
